package rtp

import (
	"bytes"
	"testing"

	"github.com/im-pingo/liveforge/pkg/avframe"
)

func TestG729Packetize(t *testing.T) {
	frameData := make([]byte, 10)
	for i := range frameData {
		frameData[i] = byte(i % 256)
	}

	frame := &avframe.AVFrame{
		MediaType: avframe.MediaTypeAudio,
		Codec:     avframe.CodecG729,
		Payload:   frameData,
	}

	p := &G729Packetizer{}
	pkts, err := p.Packetize(frame, DefaultMTU)
	if err != nil {
		t.Fatalf("Packetize: %v", err)
	}
	if len(pkts) != 1 {
		t.Fatalf("expected 1 packet, got %d", len(pkts))
	}

	pkt := pkts[0]
	if !pkt.Header.Marker {
		t.Error("expected Marker=true")
	}
	if !bytes.Equal(pkt.Payload, frameData) {
		t.Error("payload mismatch")
	}
}

func TestG729DepacketizeRoundTrip(t *testing.T) {
	frameData := make([]byte, 10)
	for i := range frameData {
		frameData[i] = byte(i % 256)
	}

	frame := &avframe.AVFrame{
		MediaType: avframe.MediaTypeAudio,
		Codec:     avframe.CodecG729,
		Payload:   frameData,
	}

	p := &G729Packetizer{}
	pkts, err := p.Packetize(frame, DefaultMTU)
	if err != nil {
		t.Fatalf("Packetize: %v", err)
	}

	d := &G729Depacketizer{}
	result, err := d.Depacketize(pkts[0])
	if err != nil {
		t.Fatalf("Depacketize: %v", err)
	}

	if !bytes.Equal(result.Payload, frameData) {
		t.Error("round-trip payload mismatch")
	}
	if result.Codec != avframe.CodecG729 {
		t.Errorf("codec: got %v, want CodecG729", result.Codec)
	}
}
