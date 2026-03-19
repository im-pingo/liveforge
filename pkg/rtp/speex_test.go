package rtp

import (
	"bytes"
	"testing"

	"github.com/im-pingo/liveforge/pkg/avframe"
)

func TestSpeexPacketize(t *testing.T) {
	frameData := make([]byte, 62)
	for i := range frameData {
		frameData[i] = byte(i % 256)
	}

	frame := &avframe.AVFrame{
		MediaType: avframe.MediaTypeAudio,
		Codec:     avframe.CodecSpeex,
		Payload:   frameData,
	}

	p := &SpeexPacketizer{}
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

func TestSpeexDepacketizeRoundTrip(t *testing.T) {
	frameData := make([]byte, 62)
	for i := range frameData {
		frameData[i] = byte(i % 256)
	}

	frame := &avframe.AVFrame{
		MediaType: avframe.MediaTypeAudio,
		Codec:     avframe.CodecSpeex,
		Payload:   frameData,
	}

	p := &SpeexPacketizer{}
	pkts, err := p.Packetize(frame, DefaultMTU)
	if err != nil {
		t.Fatalf("Packetize: %v", err)
	}

	d := &SpeexDepacketizer{}
	result, err := d.Depacketize(pkts[0])
	if err != nil {
		t.Fatalf("Depacketize: %v", err)
	}

	if !bytes.Equal(result.Payload, frameData) {
		t.Error("round-trip payload mismatch")
	}
	if result.Codec != avframe.CodecSpeex {
		t.Errorf("codec: got %v, want CodecSpeex", result.Codec)
	}
}
