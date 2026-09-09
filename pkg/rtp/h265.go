package rtp

import (
	"encoding/binary"
	"fmt"

	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/codec/h265"
	pionrtp "github.com/pion/rtp/v2"
)

const (
	// h265NALTypeAP is the NAL unit type for Aggregation Packets (RFC 7798 Section 4.4.2).
	h265NALTypeAP = 48
	// h265NALTypeFU is the NAL unit type for Fragmentation Units (RFC 7798 Section 4.4.3).
	h265NALTypeFU = 49
	// Bound incomplete NAL retention while allowing large high-resolution frames.
	h265MaxNALBytes    = 16 << 20
	h265MaxFUFragments = 16384
)

// H265Packetizer splits H.265 NAL units into RTP packets.
type H265Packetizer struct{}

// Packetize splits a single H.265 NAL unit into one or more RTP packets.
// For NAL units that fit within the MTU, a single NAL unit packet is produced.
// For larger NAL units, FU (Fragmentation Unit) packets are produced per RFC 7798.
func (p *H265Packetizer) Packetize(frame *avframe.AVFrame, mtu int) ([]*pionrtp.Packet, error) {
	if frame == nil || len(frame.Payload) < 2 {
		return nil, fmt.Errorf("h265: frame payload too short (need at least 2 bytes for NAL header)")
	}
	if mtu < 4 {
		return nil, fmt.Errorf("h265: MTU too small (minimum 4)")
	}

	nal := frame.Payload

	// Single NAL unit packet: payload fits within MTU.
	if len(nal) <= mtu {
		pkt := &pionrtp.Packet{
			Header: pionrtp.Header{
				Marker: true,
			},
			Payload: make([]byte, len(nal)),
		}
		copy(pkt.Payload, nal)
		return []*pionrtp.Packet{pkt}, nil
	}

	// FU fragmentation (RFC 7798 Section 4.4.3).
	// PayloadHdr (2 bytes) + FU header (1 byte) = 3 bytes overhead.
	maxChunk := mtu - 3
	if maxChunk <= 0 {
		return nil, fmt.Errorf("h265: MTU too small for FU fragmentation")
	}

	// Build the 2-byte PayloadHdr for FU packets:
	//   first byte: (nal[0] & 0x81) | (49 << 1)
	//   second byte: nal[1] (TID)
	payloadHdr0 := (nal[0] & 0x81) | (h265NALTypeFU << 1)
	payloadHdr1 := nal[1]

	// Original NAL type from the first byte of the NAL header.
	nalType := (nal[0] >> 1) & 0x3F

	// Skip the 2-byte NAL header; fragment the rest.
	data := nal[2:]
	var packets []*pionrtp.Packet

	for len(data) > 0 {
		chunk := data
		if len(chunk) > maxChunk {
			chunk = data[:maxChunk]
		}
		data = data[len(chunk):]

		isFirst := len(packets) == 0
		isLast := len(data) == 0

		// FU header: S bit (0x80) | E bit (0x40) | NAL type (6 bits)
		var fuHeader byte = nalType
		if isFirst {
			fuHeader |= 0x80
		}
		if isLast {
			fuHeader |= 0x40
		}

		payload := make([]byte, 3+len(chunk))
		payload[0] = payloadHdr0
		payload[1] = payloadHdr1
		payload[2] = fuHeader
		copy(payload[3:], chunk)

		pkt := &pionrtp.Packet{
			Header: pionrtp.Header{
				Marker: isLast,
			},
			Payload: payload,
		}
		packets = append(packets, pkt)
	}

	return packets, nil
}

// H265Depacketizer reassembles RTP packets into H.265 NAL units.
type H265Depacketizer struct {
	buf           []byte
	fuSequence    uint16
	fuTimestamp   uint32
	fuSSRC        uint32
	fuPayloadType uint8
	fuFragments   int
	vps           []byte
	sps           []byte
	pps           []byte
}

// Depacketize processes a single RTP packet and returns an AVFrame when a
// complete NAL unit has been reassembled. Returns (nil, nil) for intermediate
// FU fragments.
func (d *H265Depacketizer) Depacketize(pkt *pionrtp.Packet) (*avframe.AVFrame, error) {
	frames, err := d.DepacketizeFrames(pkt)
	if err != nil || len(frames) == 0 {
		return nil, err
	}
	// Preserve the original single-frame behavior for mixed APs, where media
	// took precedence over parameter sets. Batch-aware callers receive both.
	return frames[len(frames)-1], nil
}

// DepacketizeFrames processes one RTP packet and returns codec configuration
// before media when an aggregation packet contains both.
func (d *H265Depacketizer) DepacketizeFrames(pkt *pionrtp.Packet) (frames []*avframe.AVFrame, err error) {
	defer func() {
		if err != nil {
			d.resetFU()
		}
	}()
	if pkt == nil || len(pkt.Payload) < 2 {
		return nil, fmt.Errorf("h265: payload too short")
	}

	nalType := (pkt.Payload[0] >> 1) & 0x3F

	var nalus [][]byte
	switch {
	case nalType >= 0 && nalType <= 47:
		d.resetFU()
		nalus = [][]byte{append([]byte(nil), pkt.Payload...)}

	case nalType == h265NALTypeAP:
		d.resetFU()
		nalus, err = parseH265AggregationPacket(pkt.Payload)
		if err != nil {
			return nil, err
		}

	case nalType == h265NALTypeFU:
		if len(pkt.Payload) < 4 {
			return nil, fmt.Errorf("h265: FU packet too short (need a nonempty fragment)")
		}

		fuHeader := pkt.Payload[2]
		isStart := (fuHeader & 0x80) != 0
		isEnd := (fuHeader & 0x40) != 0
		origNALType := fuHeader & 0x3F
		if isStart && isEnd || origNALType > 47 || pkt.Payload[0]&0x80 != 0 || pkt.Payload[1]&7 == 0 {
			return nil, fmt.Errorf("h265: invalid FU header")
		}
		if pkt.Marker && !isEnd {
			return nil, fmt.Errorf("h265: marked FU packet lacks end bit")
		}
		nalHdr0 := (pkt.Payload[0] & 0x81) | (origNALType << 1)
		nalHdr1 := pkt.Payload[1]
		fragment := pkt.Payload[3:]

		if isStart {
			// Reconstruct the 2-byte NAL header from PayloadHdr + original NAL type.
			d.resetFU()
			if len(fragment) > h265MaxNALBytes-2 {
				return nil, fmt.Errorf("h265: fragmented NAL exceeds %d bytes", h265MaxNALBytes)
			}
			d.buf = make([]byte, 2, 2+len(fragment))
			d.buf[0] = nalHdr0
			d.buf[1] = nalHdr1
			d.fuTimestamp = pkt.Timestamp
			d.fuSSRC = pkt.SSRC
			d.fuPayloadType = pkt.PayloadType
		} else {
			if d.buf == nil {
				return nil, fmt.Errorf("h265: FU continuation without start")
			}
			if pkt.SequenceNumber != d.fuSequence+1 || pkt.Timestamp != d.fuTimestamp ||
				pkt.SSRC != d.fuSSRC || pkt.PayloadType != d.fuPayloadType ||
				nalHdr0 != d.buf[0] || nalHdr1 != d.buf[1] {
				return nil, fmt.Errorf("h265: discontinuous FU sequence or NAL identity")
			}
			if d.fuFragments >= h265MaxFUFragments {
				return nil, fmt.Errorf("h265: fragmented NAL exceeds %d fragments", h265MaxFUFragments)
			}
			if len(fragment) > h265MaxNALBytes-len(d.buf) {
				return nil, fmt.Errorf("h265: fragmented NAL exceeds %d bytes", h265MaxNALBytes)
			}
		}
		// Keep capacity as well as length within the retention ceiling.
		if size := len(d.buf) + len(fragment); size > cap(d.buf) {
			capacity := min(h265MaxNALBytes, max(size, cap(d.buf)*2))
			buf := make([]byte, len(d.buf), capacity)
			copy(buf, d.buf)
			d.buf = buf
		}
		d.buf = append(d.buf, fragment...)
		d.fuSequence = pkt.SequenceNumber
		d.fuFragments++

		if isEnd {
			payload := d.buf
			d.resetFU()
			nalus = [][]byte{payload}
		} else {
			return nil, nil
		}

	default:
		return nil, fmt.Errorf("h265: unsupported NAL type %d", nalType)
	}

	return d.normalizeNALUs(nalus), nil
}

func (d *H265Depacketizer) resetFU() {
	d.buf = nil
	d.fuSequence = 0
	d.fuTimestamp = 0
	d.fuSSRC = 0
	d.fuPayloadType = 0
	d.fuFragments = 0
}

func parseH265AggregationPacket(payload []byte) ([][]byte, error) {
	if len(payload) < 4 {
		return nil, fmt.Errorf("h265: aggregation packet too short")
	}
	var nalus [][]byte
	for offset := 2; offset < len(payload); {
		if offset+2 > len(payload) {
			return nil, fmt.Errorf("h265: truncated aggregation NAL length")
		}
		nalLen := int(binary.BigEndian.Uint16(payload[offset : offset+2]))
		offset += 2
		if nalLen < 2 || offset+nalLen > len(payload) {
			return nil, fmt.Errorf("h265: invalid aggregation NAL length %d", nalLen)
		}
		nalus = append(nalus, append([]byte(nil), payload[offset:offset+nalLen]...))
		offset += nalLen
	}
	if len(nalus) == 0 {
		return nil, fmt.Errorf("h265: empty aggregation packet")
	}
	return nalus, nil
}

func (d *H265Depacketizer) normalizeNALUs(nalus [][]byte) []*avframe.AVFrame {
	frameType := avframe.FrameTypeInterframe
	hasVCL := false
	hasParameterSets := false
	for _, nal := range nalus {
		if len(nal) < 2 {
			continue
		}
		nalType := (nal[0] >> 1) & 0x3F
		switch nalType {
		case h265.NALTypeVPS:
			d.vps = append(d.vps[:0], nal...)
			hasParameterSets = true
		case h265.NALTypeSPS:
			d.sps = append(d.sps[:0], nal...)
			hasParameterSets = true
		case h265.NALTypePPS:
			d.pps = append(d.pps[:0], nal...)
			hasParameterSets = true
		default:
			if nalType <= 31 {
				hasVCL = true
				// IRAP VCL NAL unit types are random-access pictures.
				if nalType >= 16 && nalType <= 23 {
					frameType = avframe.FrameTypeKeyframe
				}
			}
		}
	}

	var frames []*avframe.AVFrame
	if hasParameterSets {
		config := h265.BuildHVCCDecoderConfig(h265ParameterSetsAnnexB(d.vps, d.sps, d.pps))
		if config != nil {
			frames = append(frames, avframe.NewAVFrame(
				avframe.MediaTypeVideo,
				avframe.CodecH265,
				avframe.FrameTypeSequenceHeader,
				0,
				0,
				config,
			))
		}
	}

	if !hasVCL {
		return frames
	}
	payload := h265NALUsToHVCC(nalus)
	if len(payload) == 0 {
		return frames
	}
	frames = append(frames, avframe.NewAVFrame(
		avframe.MediaTypeVideo,
		avframe.CodecH265,
		frameType,
		0,
		0,
		payload,
	))
	return frames
}

func h265ParameterSetsAnnexB(vps, sps, pps []byte) []byte {
	var annexB []byte
	for _, nal := range [][]byte{vps, sps, pps} {
		if len(nal) == 0 {
			continue
		}
		annexB = append(annexB, 0, 0, 0, 1)
		annexB = append(annexB, nal...)
	}
	return annexB
}

func h265NALUsToHVCC(nalus [][]byte) []byte {
	var payload []byte
	for _, nal := range nalus {
		if len(nal) == 0 {
			continue
		}
		var size [4]byte
		binary.BigEndian.PutUint32(size[:], uint32(len(nal)))
		payload = append(payload, size[:]...)
		payload = append(payload, nal...)
	}
	return payload
}
