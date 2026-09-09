package rtp

import (
	"fmt"

	"github.com/im-pingo/liveforge/pkg/avframe"
	pioncodecs "github.com/pion/rtp/codecs"
	pionrtp "github.com/pion/rtp/v2"
)

// VP8Packetizer packetizes VP8 frames into RTP packets.
// Uses a simplified 1-byte payload descriptor per RFC 7741.
type VP8Packetizer struct{}

// Packetize splits a VP8 frame into one or more RTP packets.
// Each packet carries a 1-byte payload descriptor followed by frame data.
// If the frame fits within mtu bytes (including descriptor), a single packet is
// produced. Otherwise the frame is fragmented across multiple packets.
func (p *VP8Packetizer) Packetize(frame *avframe.AVFrame, mtu int) ([]*pionrtp.Packet, error) {
	data := frame.Payload
	if len(data) == 0 {
		return nil, fmt.Errorf("empty VP8 frame payload")
	}
	if mtu < 2 {
		return nil, fmt.Errorf("MTU too small: %d", mtu)
	}

	maxChunk := mtu - 1 // 1 byte for payload descriptor

	// Single packet case.
	if len(data) <= maxChunk {
		payload := make([]byte, 1+len(data))
		payload[0] = 0x10 // S=1, PartID=0
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
		if isFirst {
			descriptor = 0x10 // S=1
		}
		// Subsequent fragments: descriptor = 0x00 (S=0)

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

// VP8Depacketizer reassembles RTP packets into VP8 frames.
type VP8Depacketizer struct {
	buf         []byte
	sequence    uint16
	timestamp   uint32
	ssrc        uint32
	payloadType uint8
	fragments   int
}

const (
	vp8MaxFrameBytes     = 16 << 20
	vp8MaxFrameFragments = 16384
)

// Depacketize processes one RTP packet. It accumulates fragments and returns
// a completed AVFrame when the marker bit is set. Returns (nil, nil) for
// intermediate fragments.
func (d *VP8Depacketizer) Depacketize(pkt *pionrtp.Packet) (frame *avframe.AVFrame, err error) {
	defer func() {
		if err != nil {
			d.reset()
		}
	}()
	if pkt == nil {
		return nil, fmt.Errorf("VP8 RTP packet is nil")
	}
	var descriptor pioncodecs.VP8Packet
	payload, err := descriptor.Unmarshal(pkt.Payload)
	if err != nil {
		return nil, fmt.Errorf("VP8 descriptor: %w", err)
	}
	if len(payload) == 0 {
		return nil, fmt.Errorf("VP8 RTP payload is empty")
	}
	if descriptor.S == 1 && descriptor.PID == 0 {
		d.reset()
		d.timestamp, d.ssrc, d.payloadType = pkt.Timestamp, pkt.SSRC, pkt.PayloadType
	} else {
		if d.buf == nil {
			return nil, fmt.Errorf("VP8 continuation without frame start")
		}
		if pkt.SequenceNumber != d.sequence+1 || pkt.Timestamp != d.timestamp || pkt.SSRC != d.ssrc || pkt.PayloadType != d.payloadType {
			return nil, fmt.Errorf("VP8 discontinuous frame")
		}
	}
	if d.fragments >= vp8MaxFrameFragments || len(payload) > vp8MaxFrameBytes-len(d.buf) {
		return nil, fmt.Errorf("VP8 frame exceeds %d bytes or %d fragments", vp8MaxFrameBytes, vp8MaxFrameFragments)
	}
	// Bound capacity as well as length; returned frame storage is never reused.
	if size := len(d.buf) + len(payload); size > cap(d.buf) {
		capacity := min(vp8MaxFrameBytes, max(size, cap(d.buf)*2))
		buffer := make([]byte, len(d.buf), capacity)
		copy(buffer, d.buf)
		d.buf = buffer
	}
	d.buf = append(d.buf, payload...)
	d.sequence = pkt.SequenceNumber
	d.fragments++

	if pkt.Marker {
		data := d.buf
		d.reset()
		// VP8 keyframe detection: bit 0 of the first byte is the inverse keyframe flag.
		// See RFC 6386 §9.1: keyframe when (data[0] & 0x01) == 0.
		ft := avframe.FrameTypeInterframe
		if len(data) > 0 && (data[0]&0x01) == 0 {
			ft = avframe.FrameTypeKeyframe
		}
		return avframe.NewAVFrame(
			avframe.MediaTypeVideo,
			avframe.CodecVP8,
			ft,
			0, 0,
			data,
		), nil
	}

	return nil, nil
}

func (d *VP8Depacketizer) reset() {
	d.buf = nil
	d.fragments = 0
}
