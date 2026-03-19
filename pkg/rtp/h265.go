package rtp

import (
	"fmt"

	"github.com/im-pingo/liveforge/pkg/avframe"
	pionrtp "github.com/pion/rtp/v2"
)

const (
	// h265NALTypeFU is the NAL unit type for Fragmentation Units (RFC 7798 Section 4.4.3).
	h265NALTypeFU = 49
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
	buf []byte
}

// Depacketize processes a single RTP packet and returns an AVFrame when a
// complete NAL unit has been reassembled. Returns (nil, nil) for intermediate
// FU fragments.
func (d *H265Depacketizer) Depacketize(pkt *pionrtp.Packet) (*avframe.AVFrame, error) {
	if pkt == nil || len(pkt.Payload) < 2 {
		return nil, fmt.Errorf("h265: payload too short")
	}

	nalType := (pkt.Payload[0] >> 1) & 0x3F

	switch {
	case nalType >= 0 && nalType <= 47:
		// Single NAL unit packet.
		payload := make([]byte, len(pkt.Payload))
		copy(payload, pkt.Payload)
		return &avframe.AVFrame{
			MediaType: avframe.MediaTypeVideo,
			Codec:     avframe.CodecH265,
			Payload:   payload,
		}, nil

	case nalType == h265NALTypeFU:
		if len(pkt.Payload) < 3 {
			return nil, fmt.Errorf("h265: FU packet too short (need at least 3 bytes)")
		}

		fuHeader := pkt.Payload[2]
		isStart := (fuHeader & 0x80) != 0
		isEnd := (fuHeader & 0x40) != 0
		origNALType := fuHeader & 0x3F

		if isStart {
			// Reconstruct the 2-byte NAL header from PayloadHdr + original NAL type.
			// First byte: (payloadHdr[0] & 0x81) | (origNALType << 1)
			nalHdr0 := (pkt.Payload[0] & 0x81) | (origNALType << 1)
			nalHdr1 := pkt.Payload[1]

			d.buf = make([]byte, 2, 2+len(pkt.Payload)-3)
			d.buf[0] = nalHdr0
			d.buf[1] = nalHdr1
			d.buf = append(d.buf, pkt.Payload[3:]...)
		} else {
			if d.buf == nil {
				return nil, fmt.Errorf("h265: FU continuation without start")
			}
			d.buf = append(d.buf, pkt.Payload[3:]...)
		}

		if isEnd {
			payload := d.buf
			d.buf = nil
			return &avframe.AVFrame{
				MediaType: avframe.MediaTypeVideo,
				Codec:     avframe.CodecH265,
				Payload:   payload,
			}, nil
		}

		return nil, nil

	default:
		return nil, fmt.Errorf("h265: unsupported NAL type %d", nalType)
	}
}
