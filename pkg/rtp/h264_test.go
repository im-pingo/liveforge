package rtp

import (
	"bytes"
	"testing"

	"github.com/im-pingo/liveforge/pkg/avframe"
	pionrtp "github.com/pion/rtp/v2"
)

func TestH264PacketizeSingleNAL(t *testing.T) {
	nalData := make([]byte, 100)
	nalData[0] = 0x65 // IDR slice NAL type 5
	frame := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 0, 0, nalData)
	p := &H264Packetizer{}
	pkts, err := p.Packetize(frame, 1400)
	if err != nil {
		t.Fatalf("Packetize: %v", err)
	}
	if len(pkts) != 1 {
		t.Fatalf("expected 1 packet, got %d", len(pkts))
	}
	if !pkts[0].Marker {
		t.Error("expected marker bit on single NAL")
	}
	if !bytes.Equal(pkts[0].Payload, nalData) {
		t.Error("payload mismatch")
	}
}

func TestH264PacketizeFUA(t *testing.T) {
	nalData := make([]byte, 3000)
	nalData[0] = 0x65
	frame := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 0, 0, nalData)
	p := &H264Packetizer{}
	pkts, err := p.Packetize(frame, 1400)
	if err != nil {
		t.Fatalf("Packetize: %v", err)
	}
	if len(pkts) < 3 {
		t.Fatalf("expected >=3 FU-A packets, got %d", len(pkts))
	}
	// First FU-A has S bit
	if pkts[0].Payload[1]&0x80 == 0 {
		t.Error("first FU-A missing S bit")
	}
	// Last has marker
	if !pkts[len(pkts)-1].Marker {
		t.Error("last FU-A missing marker bit")
	}
	// Middle packets: no S or E bit
	for i := 1; i < len(pkts)-1; i++ {
		if pkts[i].Payload[1]&0xC0 != 0 {
			t.Errorf("middle packet %d has S or E bit set", i)
		}
	}
}

func TestH264PacketizeEmptyPayload(t *testing.T) {
	frame := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 0, 0, nil)
	p := &H264Packetizer{}
	_, err := p.Packetize(frame, 1400)
	if err == nil {
		t.Fatal("expected error for empty payload")
	}
}

func TestH264DepacketizeSingleNAL(t *testing.T) {
	d := &H264Depacketizer{}
	// Simulate single NAL packet (type 5 = IDR)
	payload := make([]byte, 100)
	payload[0] = 0x65 // NAL type 5
	pkt := &pionrtp.Packet{Header: pionrtp.Header{Marker: true}, Payload: payload}
	f, err := d.Depacketize(pkt)
	if err != nil {
		t.Fatalf("Depacketize: %v", err)
	}
	if f == nil {
		t.Fatal("expected frame for single NAL")
	}
	if !bytes.Equal(f.Payload, payload) {
		t.Error("payload mismatch")
	}
}

func TestH264DepacketizeRoundTrip(t *testing.T) {
	nalData := make([]byte, 3000)
	nalData[0] = 0x65
	for i := range nalData {
		nalData[i] = byte(i % 256)
	}
	nalData[0] = 0x65 // restore NAL header
	frame := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 0, 0, nalData)

	p := &H264Packetizer{}
	pkts, _ := p.Packetize(frame, 1400)

	d := &H264Depacketizer{}
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
	if len(result.Payload) != len(nalData) {
		t.Errorf("payload len = %d, want %d", len(result.Payload), len(nalData))
	}
	if !bytes.Equal(result.Payload, nalData) {
		t.Error("payload content mismatch after round-trip")
	}
}
