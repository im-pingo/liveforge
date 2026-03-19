package rtp

import (
	"bytes"
	"testing"

	"github.com/im-pingo/liveforge/pkg/avframe"
)

func TestVP8PacketizeSingle(t *testing.T) {
	data := make([]byte, 100)
	for i := range data {
		data[i] = byte(i % 256)
	}
	frame := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecVP8, avframe.FrameTypeKeyframe, 0, 0, data)

	p := &VP8Packetizer{}
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
	// Check S bit is set in descriptor.
	if pkts[0].Payload[0]&0x10 == 0 {
		t.Error("expected S bit set in payload descriptor")
	}
	if !bytes.Equal(pkts[0].Payload[1:], data) {
		t.Error("payload mismatch")
	}
}

func TestVP8PacketizeFragment(t *testing.T) {
	data := make([]byte, 3000)
	for i := range data {
		data[i] = byte(i % 256)
	}
	frame := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecVP8, avframe.FrameTypeKeyframe, 0, 0, data)

	p := &VP8Packetizer{}
	pkts, err := p.Packetize(frame, 1400)
	if err != nil {
		t.Fatalf("Packetize: %v", err)
	}
	if len(pkts) < 3 {
		t.Fatalf("expected >=3 packets, got %d", len(pkts))
	}
	// First packet: S bit set.
	if pkts[0].Payload[0]&0x10 == 0 {
		t.Error("first packet missing S bit")
	}
	// Subsequent packets: S bit not set.
	for i := 1; i < len(pkts); i++ {
		if pkts[i].Payload[0]&0x10 != 0 {
			t.Errorf("packet %d should not have S bit", i)
		}
	}
	// Last packet: marker set.
	if !pkts[len(pkts)-1].Marker {
		t.Error("last packet missing marker bit")
	}
	// Middle packets: no marker.
	for i := 0; i < len(pkts)-1; i++ {
		if pkts[i].Marker {
			t.Errorf("packet %d should not have marker", i)
		}
	}
}

func TestVP8DepacketizeRoundTrip(t *testing.T) {
	data := make([]byte, 3000)
	for i := range data {
		data[i] = byte(i % 256)
	}
	frame := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecVP8, avframe.FrameTypeKeyframe, 0, 0, data)

	p := &VP8Packetizer{}
	pkts, err := p.Packetize(frame, 1400)
	if err != nil {
		t.Fatalf("Packetize: %v", err)
	}

	d := &VP8Depacketizer{}
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
