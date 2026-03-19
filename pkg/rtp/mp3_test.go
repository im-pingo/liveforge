package rtp

import (
	"bytes"
	"testing"

	"github.com/im-pingo/liveforge/pkg/avframe"
)

func TestMP3Packetize(t *testing.T) {
	// 417-byte MP3 frame
	frameData := make([]byte, 417)
	for i := range frameData {
		frameData[i] = byte(i % 256)
	}

	frame := &avframe.AVFrame{
		MediaType: avframe.MediaTypeAudio,
		Codec:     avframe.CodecMP3,
		Payload:   frameData,
	}

	p := &MP3Packetizer{}
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

	// Verify 4-byte RFC 2250 header prefix is zeros
	if pkt.Payload[0] != 0 || pkt.Payload[1] != 0 || pkt.Payload[2] != 0 || pkt.Payload[3] != 0 {
		t.Errorf("expected 4-byte zero header, got %x", pkt.Payload[:4])
	}

	// Total size should be 4 (header) + 417 (frame) = 421
	if len(pkt.Payload) != 421 {
		t.Errorf("expected payload size 421, got %d", len(pkt.Payload))
	}
}

func TestMP3DepacketizeRoundTrip(t *testing.T) {
	frameData := make([]byte, 417)
	for i := range frameData {
		frameData[i] = byte(i % 256)
	}

	frame := &avframe.AVFrame{
		MediaType: avframe.MediaTypeAudio,
		Codec:     avframe.CodecMP3,
		Payload:   frameData,
	}

	p := &MP3Packetizer{}
	pkts, err := p.Packetize(frame, DefaultMTU)
	if err != nil {
		t.Fatalf("Packetize: %v", err)
	}

	d := &MP3Depacketizer{}
	result, err := d.Depacketize(pkts[0])
	if err != nil {
		t.Fatalf("Depacketize: %v", err)
	}

	if !bytes.Equal(result.Payload, frameData) {
		t.Error("round-trip payload mismatch")
	}
	if result.Codec != avframe.CodecMP3 {
		t.Errorf("codec: got %v, want CodecMP3", result.Codec)
	}
}
