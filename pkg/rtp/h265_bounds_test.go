package rtp

import (
	"bytes"
	"testing"

	pionrtp "github.com/pion/rtp/v2"
)

func h265FU(sequence uint16, flags byte, data ...byte) *pionrtp.Packet {
	return &pionrtp.Packet{
		Header:  pionrtp.Header{SequenceNumber: sequence, Timestamp: 100, SSRC: 99, PayloadType: 98, Marker: flags&0x40 != 0},
		Payload: append([]byte{49 << 1, 1, flags | 19}, data...),
	}
}

func assertH265FURecovery(t *testing.T, d *H265Depacketizer) {
	t.Helper()
	if frame, err := d.Depacketize(h265FU(30, 0x80, 0xaa)); frame != nil || err != nil {
		t.Fatalf("recovery start: frame=%v error=%v", frame, err)
	}
	frame, err := d.Depacketize(h265FU(31, 0x40, 0xbb))
	if err != nil || frame == nil || !bytes.Equal(frame.Payload, []byte{0, 0, 0, 4, 0x26, 1, 0xaa, 0xbb}) {
		t.Fatalf("recovery end: frame=%v error=%v", frame, err)
	}
}

func TestH265FURejectsDiscontinuitiesAndResets(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*pionrtp.Packet) *pionrtp.Packet
	}{
		{"gap", func(p *pionrtp.Packet) *pionrtp.Packet { p.SequenceNumber++; return p }},
		{"duplicate", func(p *pionrtp.Packet) *pionrtp.Packet { p.SequenceNumber--; return p }},
		{"reordered", func(p *pionrtp.Packet) *pionrtp.Packet { p.SequenceNumber -= 2; return p }},
		{"timestamp", func(p *pionrtp.Packet) *pionrtp.Packet { p.Timestamp++; return p }},
		{"source", func(p *pionrtp.Packet) *pionrtp.Packet { p.SSRC++; return p }},
		{"payload type", func(p *pionrtp.Packet) *pionrtp.Packet { p.PayloadType++; return p }},
		{"NAL type", func(p *pionrtp.Packet) *pionrtp.Packet { p.Payload[2] = 0x41; return p }},
		{"temporal ID", func(p *pionrtp.Packet) *pionrtp.Packet { p.Payload[1] = 2; return p }},
		{"layer ID", func(p *pionrtp.Packet) *pionrtp.Packet { p.Payload[0] |= 1; return p }},
		{"forbidden bit", func(p *pionrtp.Packet) *pionrtp.Packet { p.Payload[0] |= 0x80; return p }},
		{"zero temporal ID", func(p *pionrtp.Packet) *pionrtp.Packet { p.Payload[1] = 0; return p }},
		{"start and end", func(p *pionrtp.Packet) *pionrtp.Packet { p.Payload[2] |= 0x80; return p }},
		{"empty FU", func(p *pionrtp.Packet) *pionrtp.Packet { p.Payload = p.Payload[:3]; return p }},
		{"truncated FU header", func(p *pionrtp.Packet) *pionrtp.Packet { p.Payload = p.Payload[:2]; return p }},
		{"truncated NAL header", func(p *pionrtp.Packet) *pionrtp.Packet { p.Payload = p.Payload[:1]; return p }},
		{"missing end", func(p *pionrtp.Packet) *pionrtp.Packet { p.Payload[2] &^= 0x40; return p }},
		{"nested FU", func(p *pionrtp.Packet) *pionrtp.Packet { p.Payload[2] = 0x40 | 49; return p }},
		{"unsupported NAL", func(p *pionrtp.Packet) *pionrtp.Packet { p.Payload[0] = 50 << 1; return p }},
		{"truncated AP", func(p *pionrtp.Packet) *pionrtp.Packet { p.Payload[0] = 48 << 1; return p }},
		{"nil packet", func(p *pionrtp.Packet) *pionrtp.Packet { return nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &H265Depacketizer{}
			if _, err := d.Depacketize(h265FU(10, 0x80, 0xaa)); err != nil {
				t.Fatal(err)
			}
			if frame, err := d.Depacketize(tc.change(h265FU(11, 0x40, 0xbb))); err == nil || frame != nil {
				t.Errorf("invalid FU accepted: frame=%v error=%v", frame, err)
			}
			if d.buf != nil {
				t.Errorf("invalid FU retained %d pending bytes", len(d.buf))
			}
			if frame, err := d.Depacketize(h265FU(12, 0x40, 0xcc)); err == nil || frame != nil {
				t.Errorf("orphan end accepted after error: frame=%v error=%v", frame, err)
			}
			assertH265FURecovery(t, d)
		})
	}
}

func TestH265FUWraparoundAndRestart(t *testing.T) {
	d := &H265Depacketizer{}
	for i, p := range []*pionrtp.Packet{h265FU(65535, 0x80, 0xaa), h265FU(0, 0, 0xbb), h265FU(1, 0x40, 0xcc)} {
		frame, err := d.Depacketize(p)
		if err != nil {
			t.Fatal(err)
		}
		if i < 2 && frame != nil {
			t.Fatal("emitted unfinished NAL")
		}
		if i == 2 && (frame == nil || !bytes.Equal(frame.Payload, []byte{0, 0, 0, 5, 0x26, 1, 0xaa, 0xbb, 0xcc})) {
			t.Fatalf("invalid wraparound frame: %v", frame)
		}
	}
	if _, err := d.Depacketize(h265FU(10, 0x80, 0xff)); err != nil {
		t.Fatal(err)
	}
	assertH265FURecovery(t, d)
}

func TestH265FUCompleteNALDiscardsPending(t *testing.T) {
	d := &H265Depacketizer{}
	if _, err := d.Depacketize(h265FU(10, 0x80, 0xff)); err != nil {
		t.Fatal(err)
	}
	frame, err := d.Depacketize(&pionrtp.Packet{Payload: []byte{0x26, 1, 0xaa}})
	if err != nil || frame == nil || !bytes.Equal(frame.Payload, []byte{0, 0, 0, 3, 0x26, 1, 0xaa}) {
		t.Fatalf("single NAL recovery: frame=%v error=%v", frame, err)
	}
	if d.buf != nil {
		t.Fatalf("complete NAL retained %d pending bytes", len(d.buf))
	}
	if frame, err := d.Depacketize(h265FU(11, 0x40, 0xbb)); frame != nil || err == nil {
		t.Fatalf("stale end accepted: frame=%v error=%v", frame, err)
	}
	assertH265FURecovery(t, d)
}

func TestH265FUByteLimit(t *testing.T) {
	const limit = 16 << 20
	for _, start := range []bool{true, false} {
		d := &H265Depacketizer{}
		packet := h265FU(10, 0x80)
		packet.Payload = append(packet.Payload, make([]byte, limit-1)...)
		if !start {
			if _, err := d.Depacketize(h265FU(9, 0x80, 0xaa)); err != nil {
				t.Fatal(err)
			}
			packet.Payload[2] = 19
		}
		if frame, err := d.Depacketize(packet); frame != nil || err == nil {
			t.Fatalf("oversized NAL accepted: frame=%v error=%v", frame, err)
		}
		if d.buf != nil {
			t.Fatalf("oversized NAL retained %d pending bytes", len(d.buf))
		}
		assertH265FURecovery(t, d)
	}
	d := &H265Depacketizer{}
	if _, err := d.Depacketize(h265FU(10, 0x80, 0xaa)); err != nil {
		t.Fatal(err)
	}
	packet := h265FU(11, 0x40)
	packet.Payload = append(packet.Payload, make([]byte, limit-3)...)
	frame, err := d.Depacketize(packet)
	if err != nil || frame == nil || len(frame.Payload) != limit+4 {
		t.Fatalf("maximum-size NAL rejected: error=%v", err)
	}
}

func TestH265FUFragmentLimit(t *testing.T) {
	const limit = 16384
	for _, terminate := range []bool{false, true} {
		d := &H265Depacketizer{}
		for i := 0; i < limit; i++ {
			flags := byte(0)
			if i == 0 {
				flags = 0x80
			}
			if terminate && i == limit-1 {
				flags = 0x40
			}
			frame, err := d.Depacketize(h265FU(uint16(i), flags, 0xaa))
			if err != nil {
				t.Fatalf("fragment %d failed: %v", i, err)
			}
			if terminate && i == limit-1 && (frame == nil || len(frame.Payload) != limit+6) {
				t.Fatalf("maximum-fragment NAL not emitted: %v", frame)
			}
		}
		if !terminate {
			if frame, err := d.Depacketize(h265FU(limit, 0x40, 0xbb)); frame != nil || err == nil {
				t.Fatalf("excess fragment accepted: error=%v", err)
			}
			if d.buf != nil {
				t.Fatalf("excess fragments retained %d bytes", len(d.buf))
			}
		}
		assertH265FURecovery(t, d)
	}
}

func FuzzH265FUReassembly(f *testing.F) {
	f.Add([]byte{0x80, 0, 0x40})
	f.Add([]byte{0x80, 0xc0, 0x40})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1024 {
			t.Skip()
		}
		d := &H265Depacketizer{}
		for i, b := range data {
			packet := h265FU(uint16(i), b, b)
			packet.Timestamp += uint32(b & 1)
			_, err := d.Depacketize(packet)
			if err != nil && d.buf != nil {
				t.Fatal("error retained pending FU")
			}
			if len(d.buf) > 16<<20 {
				t.Fatal("FU exceeded memory bound")
			}
		}
		assertH265FURecovery(t, d)
	})
}
