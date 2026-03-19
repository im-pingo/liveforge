package rtp

import (
	"bytes"
	"testing"

	"github.com/im-pingo/liveforge/pkg/avframe"
)

func TestAACPacketize(t *testing.T) {
	frameData := make([]byte, 400)
	for i := range frameData {
		frameData[i] = byte(i % 256)
	}

	frame := &avframe.AVFrame{
		MediaType: avframe.MediaTypeAudio,
		Codec:     avframe.CodecAAC,
		Payload:   frameData,
	}

	p := &AACPacketizer{}
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

	// AU-headers-length should be 0x00 0x10 (16 bits).
	if pkt.Payload[0] != 0x00 || pkt.Payload[1] != 0x10 {
		t.Errorf("AU-headers-length: got [0x%02x 0x%02x], want [0x00 0x10]", pkt.Payload[0], pkt.Payload[1])
	}

	// Total payload: 2 (AU-headers-length) + 2 (AU-header) + 400 (data) = 404
	expectedSize := 4 + len(frameData)
	if len(pkt.Payload) != expectedSize {
		t.Errorf("payload size: got %d, want %d", len(pkt.Payload), expectedSize)
	}
}

func TestAACDepacketizeRoundTrip(t *testing.T) {
	frameData := make([]byte, 400)
	for i := range frameData {
		frameData[i] = byte(i % 256)
	}

	frame := &avframe.AVFrame{
		MediaType: avframe.MediaTypeAudio,
		Codec:     avframe.CodecAAC,
		Payload:   frameData,
	}

	p := &AACPacketizer{}
	pkts, err := p.Packetize(frame, DefaultMTU)
	if err != nil {
		t.Fatalf("Packetize: %v", err)
	}

	d := &AACDepacketizer{}
	result, err := d.Depacketize(pkts[0])
	if err != nil {
		t.Fatalf("Depacketize: %v", err)
	}

	if !bytes.Equal(result.Payload, frameData) {
		t.Error("round-trip payload mismatch")
	}
	if result.Codec != avframe.CodecAAC {
		t.Errorf("codec: got %v, want CodecAAC", result.Codec)
	}
}
