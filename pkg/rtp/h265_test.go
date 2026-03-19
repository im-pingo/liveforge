package rtp

import (
	"bytes"
	"testing"

	"github.com/im-pingo/liveforge/pkg/avframe"
	pionrtp "github.com/pion/rtp/v2"
)

func TestH265PacketizeSingleNAL(t *testing.T) {
	// Build a 100-byte NAL with a valid 2-byte header.
	// NAL type 19 (IDR_W_RADL): first byte = (19 << 1) = 0x26, second byte = 0x01 (TID=1).
	nal := make([]byte, 100)
	nal[0] = 0x26
	nal[1] = 0x01
	for i := 2; i < len(nal); i++ {
		nal[i] = byte(i)
	}

	frame := &avframe.AVFrame{
		MediaType: avframe.MediaTypeVideo,
		Codec:     avframe.CodecH265,
		Payload:   nal,
	}

	p := &H265Packetizer{}
	pkts, err := p.Packetize(frame, DefaultMTU)
	if err != nil {
		t.Fatalf("Packetize failed: %v", err)
	}
	if len(pkts) != 1 {
		t.Fatalf("expected 1 packet, got %d", len(pkts))
	}
	if !pkts[0].Header.Marker {
		t.Error("expected Marker=true on single NAL packet")
	}
	if !bytes.Equal(pkts[0].Payload, nal) {
		t.Error("payload does not match original NAL")
	}
}

func TestH265PacketizeFU(t *testing.T) {
	// Build a 3000-byte NAL (exceeds DefaultMTU of 1400).
	nal := make([]byte, 3000)
	nal[0] = 0x26 // NAL type 19
	nal[1] = 0x01 // TID=1
	for i := 2; i < len(nal); i++ {
		nal[i] = byte(i % 256)
	}

	frame := &avframe.AVFrame{
		MediaType: avframe.MediaTypeVideo,
		Codec:     avframe.CodecH265,
		Payload:   nal,
	}

	p := &H265Packetizer{}
	pkts, err := p.Packetize(frame, DefaultMTU)
	if err != nil {
		t.Fatalf("Packetize failed: %v", err)
	}
	if len(pkts) < 3 {
		t.Fatalf("expected at least 3 FU packets, got %d", len(pkts))
	}

	// Check first packet has S bit set.
	fuHeader := pkts[0].Payload[2]
	if fuHeader&0x80 == 0 {
		t.Error("expected S bit set on first FU packet")
	}
	if fuHeader&0x40 != 0 {
		t.Error("expected E bit clear on first FU packet")
	}
	if pkts[0].Header.Marker {
		t.Error("expected Marker=false on first FU packet")
	}

	// Check last packet has E bit set and Marker=true.
	last := pkts[len(pkts)-1]
	lastFU := last.Payload[2]
	if lastFU&0x40 == 0 {
		t.Error("expected E bit set on last FU packet")
	}
	if lastFU&0x80 != 0 {
		t.Error("expected S bit clear on last FU packet")
	}
	if !last.Header.Marker {
		t.Error("expected Marker=true on last FU packet")
	}

	// Check middle packets have neither S nor E bit.
	for i := 1; i < len(pkts)-1; i++ {
		mid := pkts[i].Payload[2]
		if mid&0x80 != 0 || mid&0x40 != 0 {
			t.Errorf("packet %d: expected neither S nor E bit set, got 0x%02x", i, mid)
		}
		if pkts[i].Header.Marker {
			t.Errorf("packet %d: expected Marker=false", i)
		}
	}

	// Verify NAL type in PayloadHdr.
	for _, pkt := range pkts {
		nalTypeInHdr := (pkt.Payload[0] >> 1) & 0x3F
		if nalTypeInHdr != h265NALTypeFU {
			t.Errorf("expected NAL type %d in PayloadHdr, got %d", h265NALTypeFU, nalTypeInHdr)
		}
	}
}

func TestH265DepacketizeRoundTrip(t *testing.T) {
	// Build a 3000-byte NAL that will be fragmented.
	nal := make([]byte, 3000)
	nal[0] = 0x26 // NAL type 19
	nal[1] = 0x01 // TID=1
	for i := 2; i < len(nal); i++ {
		nal[i] = byte(i % 256)
	}

	frame := &avframe.AVFrame{
		MediaType: avframe.MediaTypeVideo,
		Codec:     avframe.CodecH265,
		Payload:   nal,
	}

	p := &H265Packetizer{}
	pkts, err := p.Packetize(frame, DefaultMTU)
	if err != nil {
		t.Fatalf("Packetize failed: %v", err)
	}

	d := &H265Depacketizer{}
	var result *avframe.AVFrame
	for i, pkt := range pkts {
		f, err := d.Depacketize(pkt)
		if err != nil {
			t.Fatalf("Depacketize failed on packet %d: %v", i, err)
		}
		if i < len(pkts)-1 {
			if f != nil {
				t.Fatalf("expected nil frame for intermediate packet %d", i)
			}
		} else {
			if f == nil {
				t.Fatal("expected non-nil frame for last packet")
			}
			result = f
		}
	}

	if !bytes.Equal(result.Payload, nal) {
		t.Errorf("round-trip mismatch: got %d bytes, want %d bytes", len(result.Payload), len(nal))
	}
	if result.Codec != avframe.CodecH265 {
		t.Errorf("expected codec H265, got %v", result.Codec)
	}
}

func TestH265DepacketizeSingleNAL(t *testing.T) {
	// Single NAL packet depacketization.
	nal := []byte{0x26, 0x01, 0xAA, 0xBB, 0xCC}
	pkt := &pionrtp.Packet{
		Header:  pionrtp.Header{Marker: true},
		Payload: nal,
	}

	d := &H265Depacketizer{}
	f, err := d.Depacketize(pkt)
	if err != nil {
		t.Fatalf("Depacketize failed: %v", err)
	}
	if f == nil {
		t.Fatal("expected non-nil frame")
	}
	if !bytes.Equal(f.Payload, nal) {
		t.Error("payload mismatch")
	}
}
