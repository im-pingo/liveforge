package rtp

import (
	"bytes"
	"testing"

	"github.com/im-pingo/liveforge/pkg/avframe"
	pionrtp "github.com/pion/rtp/v2"
)

func h264FU(sequence uint16, flags byte, data ...byte) *pionrtp.Packet {
	return &pionrtp.Packet{
		Header:  pionrtp.Header{SequenceNumber: sequence, Timestamp: 100, SSRC: 99, PayloadType: 96, Marker: flags&0x40 != 0},
		Payload: append([]byte{0x7c, flags | 5}, data...),
	}
}

func assertH264FURecovery(t *testing.T, d *H264Depacketizer) {
	t.Helper()
	if frames, err := d.DepacketizeFrames(h264FU(30, 0x80, 0xaa)); len(frames) != 0 || err != nil {
		t.Fatalf("recovery start: frames=%v error=%v", frames, err)
	}
	frames, err := d.DepacketizeFrames(h264FU(31, 0x40, 0xbb))
	if err != nil || len(frames) != 1 || !bytes.Equal(frames[0].Payload, []byte{0, 0, 0, 3, 0x65, 0xaa, 0xbb}) {
		t.Fatalf("recovery end: frames=%v error=%v", frames, err)
	}
}

func assertH264PacketRejected(t *testing.T, d *H264Depacketizer, packet *pionrtp.Packet) {
	t.Helper()
	defer func() {
		if panicValue := recover(); panicValue != nil {
			t.Errorf("malformed RTP panicked: %v", panicValue)
		}
	}()
	if frames, err := d.DepacketizeFrames(packet); err == nil || len(frames) != 0 {
		t.Errorf("malformed RTP accepted: frame count=%d error=%v", len(frames), err)
	}
	if d.buf != nil || d.fuFragments != 0 {
		t.Errorf("malformed RTP retained %d pending bytes and %d fragments", len(d.buf), d.fuFragments)
	}
}

func TestH264STAPARejectsMalformedLengthsAndNALs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload []byte
	}{
		{"trailing length byte", []byte{0x78, 0, 1, 0x65, 0}},
		{"truncated length", []byte{0x78, 0}},
		{"truncated NAL", []byte{0x78, 0, 2, 0x65}},
		{"empty NAL", []byte{0x78, 0, 0}},
		{"empty aggregate", []byte{0x78}},
		{"nested aggregate", []byte{0x78, 0, 1, 0x78}},
		{"nested fragment", []byte{0x78, 0, 1, 0x7c}},
		{"zero NAL type", []byte{0x78, 0, 1, 0x60}},
		{"forbidden bit", []byte{0x78, 0, 1, 0xe5}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &H264Depacketizer{}
			assertH264PacketRejected(t, d, &pionrtp.Packet{Payload: tc.payload})
			assertH264FURecovery(t, d)
		})
	}
}

func TestH264FURejectsOrphans(t *testing.T) {
	for _, flags := range []byte{0, 0x40} {
		d := &H264Depacketizer{}
		assertH264PacketRejected(t, d, h264FU(10, flags, 0xcc))
		assertH264FURecovery(t, d)
	}
}

func TestH264FURejectsDiscontinuitiesAndResets(t *testing.T) {
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
		{"NAL type", func(p *pionrtp.Packet) *pionrtp.Packet { p.Payload[1] = 0x41; return p }},
		{"NAL reference priority", func(p *pionrtp.Packet) *pionrtp.Packet { p.Payload[0] = 0x5c; return p }},
		{"forbidden bit", func(p *pionrtp.Packet) *pionrtp.Packet { p.Payload[0] |= 0x80; return p }},
		{"start and end", func(p *pionrtp.Packet) *pionrtp.Packet { p.Payload[1] |= 0x80; return p }},
		{"truncated FU header", func(p *pionrtp.Packet) *pionrtp.Packet { p.Payload = p.Payload[:1]; return p }},
		{"missing end", func(p *pionrtp.Packet) *pionrtp.Packet { p.Payload[1] &^= 0x40; return p }},
		{"nested FU", func(p *pionrtp.Packet) *pionrtp.Packet { p.Payload[1] = 0x40 | 28; return p }},
		{"zero NAL type", func(p *pionrtp.Packet) *pionrtp.Packet { p.Payload[1] = 0x40; return p }},
		{"unsupported NAL", func(p *pionrtp.Packet) *pionrtp.Packet { p.Payload[0] = 0x79; return p }},
		{"truncated aggregate", func(p *pionrtp.Packet) *pionrtp.Packet { p.Payload = []byte{0x78, 0, 0, 0}; return p }},
		{"empty payload", func(p *pionrtp.Packet) *pionrtp.Packet { p.Payload = nil; return p }},
		{"nil packet", func(p *pionrtp.Packet) *pionrtp.Packet { return nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &H264Depacketizer{}
			if _, err := d.DepacketizeFrames(h264FU(10, 0x80, 0xaa)); err != nil {
				t.Fatal(err)
			}
			assertH264PacketRejected(t, d, tc.change(h264FU(11, 0x40, 0xbb)))
			assertH264PacketRejected(t, d, h264FU(12, 0x40, 0xcc))
			assertH264FURecovery(t, d)
		})
	}
}

func TestH264FUWraparoundAndRestart(t *testing.T) {
	d := &H264Depacketizer{}
	for i, packet := range []*pionrtp.Packet{h264FU(65535, 0x80, 0xaa), h264FU(0, 0, 0xbb), h264FU(1, 0x40, 0xcc)} {
		frames, err := d.DepacketizeFrames(packet)
		if err != nil {
			t.Fatal(err)
		}
		if i < 2 && len(frames) != 0 {
			t.Fatal("emitted unfinished NAL")
		}
		if i == 2 && (len(frames) != 1 || !bytes.Equal(frames[0].Payload, []byte{0, 0, 0, 4, 0x65, 0xaa, 0xbb, 0xcc})) {
			t.Fatalf("invalid wraparound frames: %v", frames)
		}
	}
	if _, err := d.DepacketizeFrames(h264FU(10, 0x80, 0xff)); err != nil {
		t.Fatal(err)
	}
	assertH264FURecovery(t, d)
}

func TestH264FUAllowsEmptyFragmentsAndIgnoresReservedBit(t *testing.T) {
	// RFC 6184 allows empty FU payloads and requires receivers to ignore R.
	d := &H264Depacketizer{}
	for i, packet := range []*pionrtp.Packet{h264FU(10, 0xa0), h264FU(11, 0x20, 0xaa), h264FU(12, 0x60)} {
		frames, err := d.DepacketizeFrames(packet)
		if err != nil {
			t.Fatal(err)
		}
		if i < 2 && len(frames) != 0 {
			t.Fatal("emitted unfinished NAL")
		}
		if i == 2 && (len(frames) != 1 || !bytes.Equal(frames[0].Payload, []byte{0, 0, 0, 2, 0x65, 0xaa})) {
			t.Fatalf("invalid frame after empty fragments: %v", frames)
		}
	}
}

func TestH264CompleteNALDiscardsPendingFU(t *testing.T) {
	for _, payload := range [][]byte{{0x65, 0xaa, 0xbb}, {0x78, 0, 3, 0x65, 0xaa, 0xbb}} {
		d := &H264Depacketizer{}
		if _, err := d.DepacketizeFrames(h264FU(10, 0x80, 0xff)); err != nil {
			t.Fatal(err)
		}
		frames, err := d.DepacketizeFrames(&pionrtp.Packet{Payload: payload})
		if err != nil || len(frames) != 1 || !bytes.Equal(frames[0].Payload, []byte{0, 0, 0, 3, 0x65, 0xaa, 0xbb}) {
			t.Fatalf("single NAL recovery: frames=%v error=%v", frames, err)
		}
		assertH264PacketRejected(t, d, h264FU(11, 0x40, 0xcc))
		assertH264FURecovery(t, d)
	}
}

func TestH264FUByteLimit(t *testing.T) {
	const limit = 16 << 20
	for _, start := range []bool{true, false} {
		d := &H264Depacketizer{}
		packet := h264FU(10, 0x80)
		packet.Payload = append(packet.Payload, bytes.Repeat([]byte{0xaa}, limit)...)
		if !start {
			if _, err := d.DepacketizeFrames(h264FU(9, 0x80, 0xaa)); err != nil {
				t.Fatal(err)
			}
			packet.Payload[1] = 5
			packet.Payload = packet.Payload[:len(packet.Payload)-1]
		}
		assertH264PacketRejected(t, d, packet)
		assertH264FURecovery(t, d)
	}
	d := &H264Depacketizer{}
	if _, err := d.DepacketizeFrames(h264FU(10, 0x80, 0xaa)); err != nil {
		t.Fatal(err)
	}
	packet := h264FU(11, 0x40)
	packet.Payload = append(packet.Payload, bytes.Repeat([]byte{0xbb}, limit-2)...)
	frames, err := d.DepacketizeFrames(packet)
	if err != nil || len(frames) != 1 || len(frames[0].Payload) != limit+4 {
		t.Fatalf("maximum-size NAL rejected: frame count=%d error=%v", len(frames), err)
	}
}

func TestH264FUFragmentLimit(t *testing.T) {
	const limit = 16384
	for _, terminate := range []bool{false, true} {
		d := &H264Depacketizer{}
		for i := 0; i < limit; i++ {
			flags := byte(0)
			if i == 0 {
				flags = 0x80
			}
			if terminate && i == limit-1 {
				flags = 0x40
			}
			frames, err := d.DepacketizeFrames(h264FU(uint16(i), flags, 0xaa))
			if err != nil {
				t.Fatalf("fragment %d failed: %v", i, err)
			}
			if terminate && i == limit-1 && (len(frames) != 1 || len(frames[0].Payload) != limit+5) {
				t.Fatalf("maximum-fragment NAL not emitted: %v", frames)
			}
		}
		if !terminate {
			assertH264PacketRejected(t, d, h264FU(limit, 0x40, 0xbb))
		}
		assertH264FURecovery(t, d)
	}
}

func TestH264SingleNALByteLimit(t *testing.T) {
	const limit = 16 << 20
	payload := bytes.Repeat([]byte{0xaa}, limit+1)
	payload[0] = 0x65
	d := &H264Depacketizer{}
	assertH264PacketRejected(t, d, &pionrtp.Packet{Payload: payload})
	assertH264FURecovery(t, d)
	frames, err := d.DepacketizeFrames(&pionrtp.Packet{Payload: payload[:limit]})
	if err != nil || len(frames) != 1 || len(frames[0].Payload) != limit+4 {
		t.Fatalf("maximum-size single NAL rejected: frame count=%d error=%v", len(frames), err)
	}
}

func TestH264STAPAOutputByteLimit(t *testing.T) {
	// 255 NALs with their AVCC prefixes take 255*65536 bytes. The final NAL
	// brings the output to 16 MiB + 3 bytes, just below the ceiling.
	payload := []byte{0x78}
	nal := bytes.Repeat([]byte{0xaa}, 65535)
	nal[0] = 0x65
	for range 255 {
		payload = append(payload, 0xff, 0xfc)
		payload = append(payload, nal[:65532]...)
	}
	payload = append(payload, 0xff, 0xff)
	payload = append(payload, nal...)
	d := &H264Depacketizer{}
	frames, err := d.DepacketizeFrames(&pionrtp.Packet{Payload: payload})
	if err != nil || len(frames) != 1 || len(frames[0].Payload) != (16<<20)+3 {
		t.Fatalf("maximum-size aggregate rejected: frame count=%d error=%v", len(frames), err)
	}
	payload = append(payload, 0, 1, 0x65)
	assertH264PacketRejected(t, d, &pionrtp.Packet{Payload: payload})
	assertH264FURecovery(t, d)
}

func TestH264MalformedAggregateDoesNotCacheParameterSets(t *testing.T) {
	d := &H264Depacketizer{}
	assertH264PacketRejected(t, d, &pionrtp.Packet{Payload: []byte{0x78, 0, 4, 0x67, 0x42, 0xc0, 0x0d, 0}})
	frames, err := d.DepacketizeFrames(&pionrtp.Packet{Payload: []byte{0x68, 0xcb, 0x83}})
	if err != nil || len(frames) != 0 {
		t.Fatalf("invalid aggregate cached SPS: frames=%v error=%v", frames, err)
	}
	frames, err = d.DepacketizeFrames(&pionrtp.Packet{Payload: []byte{0x67, 0x42, 0xc0, 0x0d}})
	if err != nil || len(frames) != 1 || frames[0].FrameType != avframe.FrameTypeSequenceHeader || !bytes.Equal(frames[0].Payload, []byte{1, 0x42, 0xc0, 0x0d, 0xff, 0xe1, 0, 4, 0x67, 0x42, 0xc0, 0x0d, 1, 0, 3, 0x68, 0xcb, 0x83}) {
		t.Fatalf("valid parameter sets did not recover: frames=%v error=%v", frames, err)
	}
}

func TestH264RejectsParameterSetsBeyondAVCConfigLength(t *testing.T) {
	for _, nalType := range []byte{0x67, 0x68} {
		payload := bytes.Repeat([]byte{0xaa}, 1<<16)
		payload[0] = nalType
		d := &H264Depacketizer{}
		assertH264PacketRejected(t, d, &pionrtp.Packet{Payload: payload})
		assertH264FURecovery(t, d)
	}
}

func TestH264FUCapacityBoundAndRelease(t *testing.T) {
	d := &H264Depacketizer{}
	fragment := bytes.Repeat([]byte{0xaa}, 1200)
	for i := 0; i < 14000; i++ {
		flags := byte(0)
		if i == 0 {
			flags = 0x80
		}
		frames, err := d.DepacketizeFrames(h264FU(uint16(i), flags, fragment...))
		if len(frames) != 0 {
			t.Fatal("emitted unfinished NAL")
		}
		if len(d.buf) > 16<<20 || cap(d.buf) > 16<<20 {
			t.Fatalf("pending FU exceeds limit: len=%d cap=%d", len(d.buf), cap(d.buf))
		}
		if err != nil {
			if i != (16<<20-1)/len(fragment) || d.buf != nil {
				t.Fatalf("unexpected bound or retained FU: fragment=%d len=%d error=%v", i, len(d.buf), err)
			}
			assertH264FURecovery(t, d)
			return
		}
	}
	t.Fatal("FU input did not reach memory bound")
}

func TestH264DepacketizeFramesPreservesSequenceHeaderAndMedia(t *testing.T) {
	d := &H264Depacketizer{}
	payload := []byte{0x78, 0, 4, 0x67, 0x42, 0xc0, 0x0d, 0, 3, 0x68, 0xcb, 0x83, 0, 3, 0x65, 0xaa, 0xbb}
	frames, err := d.DepacketizeFrames(&pionrtp.Packet{Payload: payload})
	if err != nil || len(frames) != 2 || frames[0].FrameType != avframe.FrameTypeSequenceHeader || frames[1].FrameType != avframe.FrameTypeKeyframe {
		t.Fatalf("missing ordered configuration and media: frames=%v error=%v", frames, err)
	}
	config := []byte{1, 0x42, 0xc0, 0x0d, 0xff, 0xe1, 0, 4, 0x67, 0x42, 0xc0, 0x0d, 1, 0, 3, 0x68, 0xcb, 0x83}
	media := []byte{0, 0, 0, 4, 0x67, 0x42, 0xc0, 0x0d, 0, 0, 0, 3, 0x68, 0xcb, 0x83, 0, 0, 0, 3, 0x65, 0xaa, 0xbb}
	if !bytes.Equal(frames[0].Payload, config) || !bytes.Equal(frames[1].Payload, media) {
		t.Fatal("configuration or AVCC media bytes changed")
	}
	// Callers reuse RTP packet storage; returned frames and cached parameters
	// must retain owned bytes, including after a later malformed aggregate.
	clear(payload)
	assertH264PacketRejected(t, d, &pionrtp.Packet{Payload: []byte{0x78, 0, 1, 0x65, 0}})
	if !bytes.Equal(frames[0].Payload, config) || !bytes.Equal(frames[1].Payload, media) {
		t.Fatal("packet reuse mutated returned frames")
	}
	frames, err = d.DepacketizeFrames(&pionrtp.Packet{Payload: []byte{0x67, 0x42, 0xc0, 0x1e}})
	config[3], config[11] = 0x1e, 0x1e
	if err != nil || len(frames) != 1 || !bytes.Equal(frames[0].Payload, config) {
		t.Fatalf("updated SPS lost cached PPS: frames=%v error=%v", frames, err)
	}
}

func FuzzH264DepacketizePayload(f *testing.F) {
	f.Add([]byte{0x78, 0, 1, 0x65, 0})
	f.Add([]byte{0x78, 0, 4, 0x67, 0x42, 0xc0, 0x0d, 0, 3, 0x68, 0xcb, 0x83})
	f.Add([]byte{0x7c, 0x85, 0xaa})
	f.Add([]byte{0x65, 0xaa, 0xbb})
	f.Fuzz(func(t *testing.T, payload []byte) {
		if len(payload) > 65536 {
			t.Skip()
		}
		d := &H264Depacketizer{}
		frames, err := d.DepacketizeFrames(&pionrtp.Packet{Payload: payload})
		if err != nil && (len(frames) != 0 || d.buf != nil) {
			t.Fatal("malformed packet retained output or partial FU")
		}
		for _, frame := range frames {
			if len(frame.Payload) > (16<<20)+4 || cap(frame.Payload) > (16<<20)+4 {
				t.Fatal("output exceeded memory bound")
			}
		}
		assertH264FURecovery(t, d)
	})
}

func FuzzH264FUReassembly(f *testing.F) {
	f.Add([]byte{0x80, 0, 0x40})
	f.Add([]byte{0x80, 0xc0, 0x40})
	f.Add([]byte{0x80, 1, 0x40})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1024 {
			t.Skip()
		}
		d := &H264Depacketizer{}
		for i, b := range data {
			packet := h264FU(uint16(i), b, b)
			packet.Timestamp += uint32(b & 1)
			frames, err := d.DepacketizeFrames(packet)
			if err != nil && (len(frames) != 0 || d.buf != nil) {
				t.Fatal("malformed FU retained output or pending input")
			}
			if len(d.buf) > 16<<20 || cap(d.buf) > 16<<20 || d.fuFragments > 16384 {
				t.Fatal("FU exceeded memory bound")
			}
		}
		assertH264FURecovery(t, d)
	})
}
