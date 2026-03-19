package rtp

import (
	"bytes"
	"testing"

	"github.com/im-pingo/liveforge/pkg/avframe"
)

func TestAV1PacketizeSingle(t *testing.T) {
	data := make([]byte, 100)
	for i := range data {
		data[i] = byte(i % 256)
	}
	frame := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecAV1, avframe.FrameTypeKeyframe, 0, 0, data)

	p := &AV1Packetizer{}
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
	// Check aggregation header W=1.
	if pkts[0].Payload[0] != 0x10 {
		t.Errorf("expected header 0x10, got 0x%02X", pkts[0].Payload[0])
	}
	if !bytes.Equal(pkts[0].Payload[1:], data) {
		t.Error("payload mismatch")
	}
}

func TestAV1PacketizeFragment(t *testing.T) {
	data := make([]byte, 3000)
	for i := range data {
		data[i] = byte(i % 256)
	}
	frame := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecAV1, avframe.FrameTypeKeyframe, 0, 0, data)

	p := &AV1Packetizer{}
	pkts, err := p.Packetize(frame, 1400)
	if err != nil {
		t.Fatalf("Packetize: %v", err)
	}
	if len(pkts) < 3 {
		t.Fatalf("expected >=3 packets, got %d", len(pkts))
	}
	// First packet: Z=0.
	if pkts[0].Payload[0]&0x80 != 0 {
		t.Error("first packet should not have Z bit")
	}
	// Subsequent packets: Z=1.
	for i := 1; i < len(pkts); i++ {
		if pkts[i].Payload[0]&0x80 == 0 {
			t.Errorf("packet %d missing Z bit", i)
		}
	}
	// Last packet: marker set.
	if !pkts[len(pkts)-1].Marker {
		t.Error("last packet missing marker bit")
	}
	// Non-last packets: no marker.
	for i := 0; i < len(pkts)-1; i++ {
		if pkts[i].Marker {
			t.Errorf("packet %d should not have marker", i)
		}
	}
}

func TestAV1DepacketizeRoundTrip(t *testing.T) {
	data := make([]byte, 3000)
	for i := range data {
		data[i] = byte(i % 256)
	}
	frame := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecAV1, avframe.FrameTypeKeyframe, 0, 0, data)

	p := &AV1Packetizer{}
	pkts, err := p.Packetize(frame, 1400)
	if err != nil {
		t.Fatalf("Packetize: %v", err)
	}

	d := &AV1Depacketizer{}
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
