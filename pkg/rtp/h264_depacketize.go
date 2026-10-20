package rtp

import (
	"encoding/binary"
	"fmt"

	pionrtp "github.com/pion/rtp/v2"
)

const (
	h264MaxNALBytes       = 16 << 20
	h264MaxFUFragments    = 16384
	h264MaxPayloadBytes   = h264MaxNALBytes + 4
	h264MaxParameterBytes = 1<<16 - 1 // AVCDecoderConfigurationRecord uses uint16 lengths.
)

// depacketizePayload validates one packet and returns owned AVCC data. No
// partial output or parameter sets escape a malformed aggregation packet.
func (d *H264Depacketizer) depacketizePayload(pkt *pionrtp.Packet) (data []byte, err error) {
	defer func() {
		if err != nil {
			d.resetFU()
		}
	}()
	if pkt == nil || len(pkt.Payload) == 0 {
		return nil, fmt.Errorf("h264: empty RTP payload")
	}
	if pkt.Payload[0]&0x80 != 0 {
		return nil, fmt.Errorf("h264: forbidden NAL bit set")
	}

	nalType := pkt.Payload[0] & 0x1f
	switch {
	case nalType > 0 && nalType < 24:
		d.resetFU()
		if err := validateH264NAL(pkt.Payload); err != nil {
			return nil, err
		}
		return h264NALToAVCC(pkt.Payload), nil
	case nalType == 24:
		d.resetFU()
		return parseH264STAPA(pkt.Payload)
	case nalType == 28:
		if len(pkt.Payload) < 2 {
			return nil, fmt.Errorf("h264: truncated FU-A header")
		}
		fuHeader := pkt.Payload[1]
		isStart := fuHeader&0x80 != 0
		isEnd := fuHeader&0x40 != 0
		origNALType := fuHeader & 0x1f
		if isStart && isEnd || origNALType == 0 || origNALType >= 24 {
			return nil, fmt.Errorf("h264: invalid FU-A header")
		}
		if pkt.Marker && !isEnd {
			return nil, fmt.Errorf("h264: marked FU-A packet lacks end bit")
		}
		// RFC 6184 permits empty FU payloads and requires receivers to ignore R.
		nalHeader := pkt.Payload[0]&0x60 | origNALType
		fragment := pkt.Payload[2:]
		if isStart {
			d.resetFU()
			if len(fragment) > h264MaxNALBytes-1 {
				return nil, fmt.Errorf("h264: fragmented NAL exceeds %d bytes", h264MaxNALBytes)
			}
			d.buf = make([]byte, 1, 1+len(fragment))
			d.buf[0] = nalHeader
			d.fuTimestamp = pkt.Timestamp
			d.fuSSRC = pkt.SSRC
			d.fuPayloadType = pkt.PayloadType
		} else {
			if d.buf == nil {
				return nil, fmt.Errorf("h264: FU-A continuation without start")
			}
			if pkt.SequenceNumber != d.fuSequence+1 || pkt.Timestamp != d.fuTimestamp ||
				pkt.SSRC != d.fuSSRC || pkt.PayloadType != d.fuPayloadType || nalHeader != d.buf[0] {
				return nil, fmt.Errorf("h264: discontinuous FU-A sequence or NAL identity")
			}
			if d.fuFragments >= h264MaxFUFragments {
				return nil, fmt.Errorf("h264: fragmented NAL exceeds %d fragments", h264MaxFUFragments)
			}
			if len(fragment) > h264MaxNALBytes-len(d.buf) {
				return nil, fmt.Errorf("h264: fragmented NAL exceeds %d bytes", h264MaxNALBytes)
			}
		}
		// Bound capacity as well as length, including Go's append growth.
		if size := len(d.buf) + len(fragment); size > cap(d.buf) {
			capacity := min(h264MaxNALBytes, max(size, cap(d.buf)*2))
			buf := make([]byte, len(d.buf), capacity)
			copy(buf, d.buf)
			d.buf = buf
		}
		d.buf = append(d.buf, fragment...)
		d.fuSequence = pkt.SequenceNumber
		d.fuFragments++
		if !isEnd {
			return nil, nil
		}
		nal := d.buf
		d.resetFU()
		if err := validateH264NAL(nal); err != nil {
			return nil, err
		}
		return h264NALToAVCC(nal), nil
	default:
		return nil, fmt.Errorf("h264: unsupported NAL type %d", nalType)
	}
}

func (d *H264Depacketizer) resetFU() {
	d.buf = nil
	d.fuSequence = 0
	d.fuTimestamp = 0
	d.fuSSRC = 0
	d.fuPayloadType = 0
	d.fuFragments = 0
}

func validateH264NAL(nal []byte) error {
	if len(nal) == 0 || nal[0]&0x80 != 0 || nal[0]&0x1f == 0 || nal[0]&0x1f >= 24 {
		return fmt.Errorf("h264: invalid NAL header")
	}
	if len(nal) > h264MaxNALBytes {
		return fmt.Errorf("h264: NAL exceeds %d bytes", h264MaxNALBytes)
	}
	if (nal[0]&0x1f == 7 || nal[0]&0x1f == 8) && len(nal) > h264MaxParameterBytes {
		return fmt.Errorf("h264: parameter set exceeds %d bytes", h264MaxParameterBytes)
	}
	return nil
}

func h264NALToAVCC(nal []byte) []byte {
	data := make([]byte, 4+len(nal))
	binary.BigEndian.PutUint32(data[:4], uint32(len(nal))) // #nosec G115 -- Every caller first validates the NAL's 16 MiB limit with validateH264NAL.
	copy(data[4:], nal)
	return data
}

func parseH264STAPA(payload []byte) ([]byte, error) {
	if len(payload) < 4 {
		return nil, fmt.Errorf("h264: aggregation packet too short")
	}
	// Validate and measure before allocating: small aggregated NALs gain two
	// bytes each when their RTP uint16 lengths become AVCC uint32 lengths.
	size := 0
	for offset := 1; offset < len(payload); {
		if len(payload)-offset < 2 {
			return nil, fmt.Errorf("h264: truncated aggregation NAL length")
		}
		nalLen := int(binary.BigEndian.Uint16(payload[offset : offset+2]))
		offset += 2
		if nalLen == 0 || nalLen > len(payload)-offset {
			return nil, fmt.Errorf("h264: invalid aggregation NAL length %d", nalLen)
		}
		if err := validateH264NAL(payload[offset : offset+nalLen]); err != nil {
			return nil, err
		}
		if nalLen+4 > h264MaxPayloadBytes-size {
			return nil, fmt.Errorf("h264: aggregate output exceeds %d bytes", h264MaxPayloadBytes)
		}
		size += 4 + nalLen
		offset += nalLen
	}
	data := make([]byte, size)
	for offset, out := 1, 0; offset < len(payload); {
		nalSize := binary.BigEndian.Uint16(payload[offset : offset+2])
		nalLen := int(nalSize)
		offset += 2
		binary.BigEndian.PutUint32(data[out:out+4], uint32(nalSize))
		copy(data[out+4:], payload[offset:offset+nalLen])
		out += 4 + nalLen
		offset += nalLen
	}
	return data, nil
}
