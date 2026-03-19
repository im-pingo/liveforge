package rtp

import (
	"bytes"
	"testing"

	"github.com/im-pingo/liveforge/pkg/avframe"
)

func TestOpusPacketize(t *testing.T) {
	frameData := make([]byte, 160)
	for i := range frameData {
		frameData[i] = byte(i % 256)
	}

	frame := &avframe.AVFrame{
		MediaType: avframe.MediaTypeAudio,
		Codec:     avframe.CodecOpus,
		Payload:   frameData,
	}

	p := &OpusPacketizer{}
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

func TestOpusDepacketizeRoundTrip(t *testing.T) {
	frameData := make([]byte, 160)
	for i := range frameData {
		frameData[i] = byte(i % 256)
	}

	frame := &avframe.AVFrame{
		MediaType: avframe.MediaTypeAudio,
		Codec:     avframe.CodecOpus,
		Payload:   frameData,
	}

	p := &OpusPacketizer{}
	pkts, err := p.Packetize(frame, DefaultMTU)
	if err != nil {
		t.Fatalf("Packetize: %v", err)
	}

	d := &OpusDepacketizer{}
	result, err := d.Depacketize(pkts[0])
	if err != nil {
		t.Fatalf("Depacketize: %v", err)
	}

	if !bytes.Equal(result.Payload, frameData) {
		t.Error("round-trip payload mismatch")
	}
	if result.Codec != avframe.CodecOpus {
		t.Errorf("codec: got %v, want CodecOpus", result.Codec)
	}
}
