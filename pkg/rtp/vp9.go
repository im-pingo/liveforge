package rtp

import (
	"fmt"

	"github.com/im-pingo/liveforge/pkg/avframe"
	pionrtp "github.com/pion/rtp/v2"
)

// VP9Packetizer packetizes VP9 frames into RTP packets.
// Uses a simplified 1-byte payload descriptor per draft-ietf-payload-vp9.
type VP9Packetizer struct{}

// Packetize splits a VP9 frame into one or more RTP packets.
// Each packet carries a 1-byte payload descriptor followed by frame data.
// Descriptor bits: I=0, P=0, L=0, F=0, B=beginning, E=end, V=0, Z=0.
// B is bit 3 (0x08), E is bit 2 (0x04).
func (p *VP9Packetizer) Packetize(frame *avframe.AVFrame, mtu int) ([]*pionrtp.Packet, error) {
	data := frame.Payload
	if len(data) == 0 {
		return nil, fmt.Errorf("empty VP9 frame payload")
	}
	if mtu < 2 {
		return nil, fmt.Errorf("MTU too small: %d", mtu)
	}

	maxChunk := mtu - 1 // 1 byte for payload descriptor

	// Single packet case.
	if len(data) <= maxChunk {
		payload := make([]byte, 1+len(data))
		payload[0] = 0x0C // B=1 E=1
		copy(payload[1:], data)
		pkt := &pionrtp.Packet{
			Header:  pionrtp.Header{Marker: true},
			Payload: payload,
		}
		return []*pionrtp.Packet{pkt}, nil
	}

	// Fragmentation.
	var pkts []*pionrtp.Packet
	offset := 0
	for offset < len(data) {
		end := offset + maxChunk
		if end > len(data) {
			end = len(data)
		}

		isFirst := offset == 0
		isLast := end == len(data)

		var descriptor byte
		if isFirst && isLast {
			descriptor = 0x0C // B=1 E=1
		} else if isFirst {
			descriptor = 0x08 // B=1 E=0
		} else if isLast {
			descriptor = 0x04 // B=0 E=1
		}
		// Middle fragments: descriptor = 0x00

		payload := make([]byte, 1+end-offset)
		payload[0] = descriptor
		copy(payload[1:], data[offset:end])

		pkt := &pionrtp.Packet{
			Header:  pionrtp.Header{Marker: isLast},
			Payload: payload,
		}
		pkts = append(pkts, pkt)
		offset = end
	}

	return pkts, nil
}

// VP9Depacketizer reassembles RTP packets into VP9 frames.
type VP9Depacketizer struct {
	buf []byte
}

// Depacketize processes one RTP packet. It accumulates fragments and returns
// a completed AVFrame when the E bit is set. Returns (nil, nil) for
// intermediate fragments.
func (d *VP9Depacketizer) Depacketize(pkt *pionrtp.Packet) (*avframe.AVFrame, error) {
	payload := pkt.Payload
	if len(payload) < 2 {
		return nil, fmt.Errorf("VP9 RTP payload too short")
	}

	descriptor := payload[0]
	bBit := descriptor & 0x08
	eBit := descriptor & 0x04

	if bBit != 0 {
		// Beginning of a new frame.
		d.buf = make([]byte, 0, len(payload)*4)
		d.buf = append(d.buf, payload[1:]...)
	} else {
		// Continuation fragment.
		d.buf = append(d.buf, payload[1:]...)
	}

	if eBit != 0 {
		data := d.buf
		d.buf = nil
		// VP9 keyframe detection: the P bit (0x40) in the first descriptor byte
		// indicates an inter-prediction frame. Keyframes have P=0.
		// See draft-ietf-payload-vp9 §4.2.
		ft := avframe.FrameTypeInterframe
		if len(payload) > 0 && (payload[0]&0x40) == 0 {
			ft = avframe.FrameTypeKeyframe
		}
		return avframe.NewAVFrame(
			avframe.MediaTypeVideo,
			avframe.CodecVP9,
			ft,
			0, 0,
			data,
		), nil
	}

	return nil, nil
}
