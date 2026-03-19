package rtp

import (
	"fmt"

	"github.com/im-pingo/liveforge/pkg/avframe"
	pionrtp "github.com/pion/rtp/v2"
)

// AV1Packetizer packetizes AV1 OBU data into RTP packets.
// Uses a simplified aggregation header per draft-ietf-avt-rtp-av1.
type AV1Packetizer struct{}

// Packetize splits AV1 OBU data into one or more RTP packets.
// Each packet carries a 1-byte aggregation header followed by OBU data.
//
// Single OBU that fits in MTU: header = 0x10 (W=1), Marker=true.
// Fragmented: first fragment header = 0x00 (Z=0), continuations = 0x80 (Z=1).
// Last fragment gets Marker=true.
func (p *AV1Packetizer) Packetize(frame *avframe.AVFrame, mtu int) ([]*pionrtp.Packet, error) {
	data := frame.Payload
	if len(data) == 0 {
		return nil, fmt.Errorf("empty AV1 frame payload")
	}
	if mtu < 2 {
		return nil, fmt.Errorf("MTU too small: %d", mtu)
	}

	maxChunk := mtu - 1 // 1 byte for aggregation header

	// Single packet case.
	if len(data) <= maxChunk {
		payload := make([]byte, 1+len(data))
		payload[0] = 0x10 // W=1
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

		var header byte
		if isFirst {
			header = 0x00 // Z=0
		} else {
			header = 0x80 // Z=1 (continuation)
		}

		payload := make([]byte, 1+end-offset)
		payload[0] = header
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

// AV1Depacketizer reassembles RTP packets into AV1 frames.
type AV1Depacketizer struct {
	buf []byte
}

// Depacketize processes one RTP packet. It accumulates fragments and returns
// a completed AVFrame when the marker bit is set. Returns (nil, nil) for
// intermediate fragments.
func (d *AV1Depacketizer) Depacketize(pkt *pionrtp.Packet) (*avframe.AVFrame, error) {
	payload := pkt.Payload
	if len(payload) < 2 {
		return nil, fmt.Errorf("AV1 RTP payload too short")
	}

	header := payload[0]
	zBit := header & 0x80

	if zBit == 0 {
		// Start of a new OBU / frame.
		d.buf = make([]byte, 0, len(payload)*4)
		d.buf = append(d.buf, payload[1:]...)
	} else {
		// Continuation fragment.
		d.buf = append(d.buf, payload[1:]...)
	}

	if pkt.Marker {
		data := d.buf
		d.buf = nil
		return avframe.NewAVFrame(
			avframe.MediaTypeVideo,
			avframe.CodecAV1,
			avframe.FrameTypeInterframe,
			0, 0,
			data,
		), nil
	}

	return nil, nil
}
