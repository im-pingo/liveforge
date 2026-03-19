package rtp

import (
	"fmt"

	"github.com/im-pingo/liveforge/pkg/avframe"
	pionrtp "github.com/pion/rtp/v2"
)

// H264Packetizer packetizes H.264 NAL units into RTP packets.
// Supports single NAL unit mode and FU-A fragmentation.
type H264Packetizer struct{}

// Packetize splits a single NAL unit into one or more RTP packets.
// If the NAL fits within mtu bytes, a single NAL packet is produced.
// Otherwise FU-A fragmentation is used.
func (p *H264Packetizer) Packetize(frame *avframe.AVFrame, mtu int) ([]*pionrtp.Packet, error) {
	nal := frame.Payload
	if len(nal) == 0 {
		return nil, fmt.Errorf("empty NAL unit payload")
	}
	if mtu < 3 {
		return nil, fmt.Errorf("MTU too small: %d", mtu)
	}

	// Single NAL unit mode — fits in one packet.
	if len(nal) <= mtu {
		pkt := &pionrtp.Packet{
			Header:  pionrtp.Header{Marker: true},
			Payload: make([]byte, len(nal)),
		}
		copy(pkt.Payload, nal)
		return []*pionrtp.Packet{pkt}, nil
	}

	// FU-A fragmentation.
	nalHeader := nal[0]
	nalType := nalHeader & 0x1F
	fuIndicator := (nalHeader & 0xE0) | 28 // NRI bits + FU-A type (28)

	// Each FU-A packet carries: fu_indicator (1) + fu_header (1) + chunk data.
	maxChunk := mtu - 2
	nalBody := nal[1:] // skip the NAL header byte (reconstructed via FU indicator/header)

	var pkts []*pionrtp.Packet
	offset := 0
	for offset < len(nalBody) {
		end := offset + maxChunk
		if end > len(nalBody) {
			end = len(nalBody)
		}

		isFirst := offset == 0
		isLast := end == len(nalBody)

		var fuHeader byte = nalType
		if isFirst {
			fuHeader |= 0x80 // S bit
		}
		if isLast {
			fuHeader |= 0x40 // E bit
		}

		payload := make([]byte, 2+end-offset)
		payload[0] = fuIndicator
		payload[1] = fuHeader
		copy(payload[2:], nalBody[offset:end])

		pkt := &pionrtp.Packet{
			Header:  pionrtp.Header{Marker: isLast},
			Payload: payload,
		}
		pkts = append(pkts, pkt)
		offset = end
	}

	return pkts, nil
}

// H264Depacketizer reassembles RTP packets into H.264 NAL units.
// Supports single NAL unit packets and FU-A fragmented packets.
type H264Depacketizer struct {
	buf []byte // accumulation buffer for FU-A reassembly
}

// Depacketize processes one RTP packet. For single NAL packets it returns an
// AVFrame immediately. For FU-A packets it accumulates fragments and returns
// a completed AVFrame when the End bit is set. Returns (nil, nil) for
// intermediate FU-A fragments.
func (d *H264Depacketizer) Depacketize(pkt *pionrtp.Packet) (*avframe.AVFrame, error) {
	payload := pkt.Payload
	if len(payload) == 0 {
		return nil, fmt.Errorf("empty RTP payload")
	}

	nalType := payload[0] & 0x1F

	switch {
	case nalType >= 1 && nalType <= 23:
		// Single NAL unit packet.
		data := make([]byte, len(payload))
		copy(data, payload)
		return avframe.NewAVFrame(
			avframe.MediaTypeVideo,
			avframe.CodecH264,
			frameTypeFromNAL(nalType),
			0, 0,
			data,
		), nil

	case nalType == 28:
		// FU-A
		if len(payload) < 2 {
			return nil, fmt.Errorf("FU-A packet too short")
		}
		fuHeader := payload[1]
		startBit := fuHeader & 0x80
		endBit := fuHeader & 0x40
		origNALType := fuHeader & 0x1F

		if startBit != 0 {
			// Reconstruct the original NAL header byte.
			reconstructed := (payload[0] & 0xE0) | origNALType
			d.buf = make([]byte, 0, len(payload)*4) // rough pre-alloc
			d.buf = append(d.buf, reconstructed)
			d.buf = append(d.buf, payload[2:]...)
		} else {
			// Middle or end fragment — append data.
			d.buf = append(d.buf, payload[2:]...)
		}

		if endBit != 0 {
			data := d.buf
			d.buf = nil
			return avframe.NewAVFrame(
				avframe.MediaTypeVideo,
				avframe.CodecH264,
				frameTypeFromNAL(origNALType),
				0, 0,
				data,
			), nil
		}
		return nil, nil

	default:
		return nil, fmt.Errorf("unsupported H.264 NAL type: %d", nalType)
	}
}

// frameTypeFromNAL returns a FrameType based on the H.264 NAL unit type.
func frameTypeFromNAL(nalType byte) avframe.FrameType {
	if nalType == 5 { // IDR slice
		return avframe.FrameTypeKeyframe
	}
	return avframe.FrameTypeInterframe
}
