package rtp

import (
	"bytes"
	"testing"

	"github.com/im-pingo/liveforge/pkg/avframe"
)

func TestVP9PacketizeSingle(t *testing.T) {
	data := make([]byte, 100)
	for i := range data {
		data[i] = byte(i % 256)
	}
	frame := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecVP9, avframe.FrameTypeKeyframe, 0, 0, data)

	p := &VP9Packetizer{}
	pkts, err := p.Packetize(frame, 1400)
	if err != nil {
		t.Fatalf("Packetize: %v", err)
	}
	if len(pkts) != 1 {
		t.Fatalf("expected 1 packet, got %d", len(pkts))
	}
	if !pkts[0].Marker {
		t.Error("expected marker bit on single packet")
	}
	// Check B and E bits are set.
	if pkts[0].Payload[0] != 0x0C {
		t.Errorf("expected descriptor 0x0C, got 0x%02X", pkts[0].Payload[0])
	}
	if !bytes.Equal(pkts[0].Payload[1:], data) {
		t.Error("payload mismatch")
	}
}

func TestVP9PacketizeFragment(t *testing.T) {
	data := make([]byte, 3000)
	for i := range data {
		data[i] = byte(i % 256)
	}
	frame := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecVP9, avframe.FrameTypeKeyframe, 0, 0, data)

	p := &VP9Packetizer{}
	pkts, err := p.Packetize(frame, 1400)
	if err != nil {
		t.Fatalf("Packetize: %v", err)
	}
	if len(pkts) < 3 {
		t.Fatalf("expected >=3 packets, got %d", len(pkts))
	}
	// First packet: B=1, E=0.
	if pkts[0].Payload[0]&0x08 == 0 {
		t.Error("first packet missing B bit")
	}
	if pkts[0].Payload[0]&0x04 != 0 {
		t.Error("first packet should not have E bit")
	}
	// Last packet: B=0, E=1.
	last := pkts[len(pkts)-1]
	if last.Payload[0]&0x08 != 0 {
		t.Error("last packet should not have B bit")
	}
	if last.Payload[0]&0x04 == 0 {
		t.Error("last packet missing E bit")
	}
	if !last.Marker {
		t.Error("last packet missing marker bit")
	}
	// Middle packets: B=0, E=0.
	for i := 1; i < len(pkts)-1; i++ {
		if pkts[i].Payload[0]&0x0C != 0 {
			t.Errorf("middle packet %d has B or E bit set", i)
		}
	}
}

func TestVP9DepacketizeRoundTrip(t *testing.T) {
	data := make([]byte, 3000)
	for i := range data {
		data[i] = byte(i % 256)
	}
	frame := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecVP9, avframe.FrameTypeKeyframe, 0, 0, data)

	p := &VP9Packetizer{}
	pkts, err := p.Packetize(frame, 1400)
	if err != nil {
		t.Fatalf("Packetize: %v", err)
	}

	d := &VP9Depacketizer{}
	var result *avframe.AVFrame
	for _, pkt := range pkts {
		f, err := d.Depacketize(pkt)
		if err != nil {
			t.Fatalf("Depacketize: %v", err)
		}
		if f != nil {
			result = f
		}
	}
	if result == nil {
		t.Fatal("no frame reassembled")
	}
	if !bytes.Equal(result.Payload, data) {
		t.Errorf("payload mismatch after round-trip: got %d bytes, want %d", len(result.Payload), len(data))
	}
}
