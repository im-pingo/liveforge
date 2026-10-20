package rtp

import (
	"bytes"
	"testing"

	"github.com/im-pingo/liveforge/pkg/avframe"
	pionrtp "github.com/pion/rtp/v2"
)

func accessUnitPacket(sequence uint16, timestamp uint32, marker bool, payload ...byte) *pionrtp.Packet {
	return &pionrtp.Packet{Header: pionrtp.Header{SequenceNumber: sequence, Timestamp: timestamp, SSRC: 99, PayloadType: 96, Marker: marker}, Payload: payload}
}

func TestAccessUnitFreshTimestampResynchronizesSequence(t *testing.T) {
	a := NewAccessUnitAssembler(avframe.CodecH264, &H264Depacketizer{})
	if _, err := a.Push(accessUnitPacket(1, 100, false, 0x65, 0xaa)); err != nil {
		t.Fatal(err)
	}
	frames, err := a.Push(accessUnitPacket(40000, 200, true, 0x65, 0xbb))
	if err != nil || len(frames) != 1 || !bytes.Equal(frames[0].Payload, []byte{0, 0, 0, 2, 0x65, 0xbb}) {
		t.Fatalf("fresh timestamp failed to recover after a large sequence gap: frames=%v error=%v", frames, err)
	}
	if frames, err := a.Push(accessUnitPacket(2, 100, true, 0x65, 0xcc)); err == nil || len(frames) != 0 {
		t.Fatalf("late old timestamp was published: frames=%v error=%v", frames, err)
	}
}

func requireAccessUnitRecovery(t *testing.T, a *AccessUnitAssembler, sequence uint16, timestamp uint32) {
	t.Helper()
	frames, err := a.Push(accessUnitPacket(sequence, timestamp, true, 0x65, 0xcc))
	if err != nil || len(frames) != 1 || !bytes.Equal(frames[0].Payload, []byte{0, 0, 0, 2, 0x65, 0xcc}) {
		t.Fatalf("clean access unit did not recover: frames=%v error=%v", frames, err)
	}
}

func TestAccessUnitDropsDamageAndSameTimestampRemainder(t *testing.T) {
	for _, tc := range []struct {
		name   string
		packet *pionrtp.Packet
	}{
		{"malformed aggregation", accessUnitPacket(11, 100, false, 0x78, 0, 1, 0x65, 0)},
		{"sequence gap", accessUnitPacket(12, 100, false, 0x65, 0xbb)},
		{"duplicate", accessUnitPacket(10, 100, false, 0x65, 0xbb)},
		{"reordered", accessUnitPacket(9, 100, false, 0x65, 0xbb)},
		{"nil packet", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := NewAccessUnitAssembler(avframe.CodecH264, &H264Depacketizer{})
			if frames, err := a.Push(accessUnitPacket(10, 100, false, 0x65, 0xaa)); err != nil || len(frames) != 0 {
				t.Fatalf("first NAL: frames=%v error=%v", frames, err)
			}
			if frames, err := a.Push(tc.packet); err == nil || len(frames) != 0 || a.payload != nil {
				t.Fatalf("damaged access unit retained/emitted media: frames=%v pending=%d error=%v", frames, len(a.payload), err)
			}
			if frames, err := a.Push(accessUnitPacket(13, 100, true, 0x65, 0xdd)); err == nil || len(frames) != 0 {
				t.Fatalf("damaged timestamp resumed at Marker: frames=%v error=%v", frames, err)
			}
			requireAccessUnitRecovery(t, a, 14, 200)
		})
	}
}

func TestAccessUnitFreshIdentityDropsUnfinishedMedia(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*pionrtp.Packet)
	}{
		{"timestamp", func(p *pionrtp.Packet) { p.Timestamp++ }},
		{"SSRC", func(p *pionrtp.Packet) { p.SSRC++ }},
		{"payload type", func(p *pionrtp.Packet) { p.PayloadType++ }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := NewAccessUnitAssembler(avframe.CodecH264, &H264Depacketizer{})
			if _, err := a.Push(accessUnitPacket(10, 100, false, 0x65, 0xaa)); err != nil {
				t.Fatal(err)
			}
			packet := accessUnitPacket(11, 100, true, 0x65, 0xbb)
			tc.change(packet)
			frames, err := a.Push(packet)
			if err != nil || len(frames) != 1 || !bytes.Equal(frames[0].Payload, []byte{0, 0, 0, 2, 0x65, 0xbb}) {
				t.Fatalf("fresh identity included abandoned bytes: frames=%v error=%v", frames, err)
			}
		})
	}
}

func TestAccessUnitDoesNotFlushUnfinishedMediaOnSequenceHeader(t *testing.T) {
	a := NewAccessUnitAssembler(avframe.CodecH264, &H264Depacketizer{})
	if _, err := a.Push(accessUnitPacket(10, 100, false, 0x65, 0xaa)); err != nil {
		t.Fatal(err)
	}
	frames, err := a.Push(accessUnitPacket(11, 100, false, 0x78, 0, 4, 0x67, 0x42, 0xc0, 0x0d, 0, 3, 0x68, 0xcb, 0x83))
	if err != nil || len(frames) != 1 || frames[0].FrameType != avframe.FrameTypeSequenceHeader {
		t.Fatalf("parameter sets flushed unfinished media: frames=%v error=%v", frames, err)
	}
	frames, err = a.Push(accessUnitPacket(12, 100, true, 0x65, 0xbb))
	if err != nil || len(frames) != 1 || frames[0].FrameType != avframe.FrameTypeKeyframe || !bytes.Equal(frames[0].Payload, []byte{0, 0, 0, 2, 0x65, 0xaa, 0, 0, 0, 2, 0x65, 0xbb}) {
		t.Fatalf("complete media did not follow header intact: frames=%v error=%v", frames, err)
	}
}

func TestAccessUnitRejectsAbandonedCodecFragment(t *testing.T) {
	for _, replacement := range [][]byte{{0x65, 0xbb}, {0x7c, 0x85, 0xbb}} {
		dp := &H264Depacketizer{}
		a := NewAccessUnitAssembler(avframe.CodecH264, dp)
		if _, err := a.Push(accessUnitPacket(10, 100, false, 0x7c, 0x85, 0xaa)); err != nil {
			t.Fatal(err)
		}
		if frames, err := a.Push(accessUnitPacket(11, 100, false, replacement...)); err == nil || len(frames) != 0 {
			t.Fatalf("new NAL hid incomplete FU: frames=%v error=%v", frames, err)
		}
		if dp.buf != nil {
			t.Fatalf("abandoned codec retained %d bytes through caller reference", len(dp.buf))
		}
		if frames, err := a.Push(accessUnitPacket(12, 100, true, 0x65, 0xdd)); err == nil || len(frames) != 0 {
			t.Fatalf("abandoned FU timestamp emitted remainder: frames=%v error=%v", frames, err)
		}
		requireAccessUnitRecovery(t, a, 13, 200)
	}
}

func TestAccessUnitOutputByteAndCapacityBound(t *testing.T) {
	const limit = (16 << 20) + 4
	a := NewAccessUnitAssembler(avframe.CodecH264, &H264Depacketizer{})
	payload := bytes.Repeat([]byte{0xaa}, 1200)
	payload[0] = 0x65
	var sequence uint16
	for ; int(sequence) <= limit/(len(payload)+4); sequence++ {
		frames, err := a.Push(accessUnitPacket(sequence, 100, false, payload...))
		if len(frames) != 0 {
			t.Fatal("unmarked NALs emitted media")
		}
		if cap(a.payload) > limit || len(a.payload) > limit {
			t.Fatalf("access unit exceeded byte limit: len=%d cap=%d", len(a.payload), cap(a.payload))
		}
		if err != nil {
			if int(sequence) != limit/(len(payload)+4) || a.payload != nil {
				t.Fatalf("unexpected byte bound: sequence=%d error=%v", sequence, err)
			}
			break
		}
	}
	if frames, err := a.Push(accessUnitPacket(sequence+1, 100, true, 0x65, 0xdd)); err == nil || len(frames) != 0 {
		t.Fatalf("oversized timestamp resumed: frames=%v error=%v", frames, err)
	}
	requireAccessUnitRecovery(t, a, sequence+2, 200)

	a = NewAccessUnitAssembler(avframe.CodecH264, &H264Depacketizer{})
	payload = bytes.Repeat([]byte{0xbb}, limit-4)
	payload[0] = 0x65
	frames, err := a.Push(accessUnitPacket(0, 100, true, payload...))
	if err != nil || len(frames) != 1 || len(frames[0].Payload) != limit || cap(frames[0].Payload) != limit || a.payload != nil {
		t.Fatalf("exact byte limit rejected or retained: frames=%d error=%v", len(frames), err)
	}
}

func TestAccessUnitIncomingPacketBound(t *testing.T) {
	for _, complete := range []bool{false, true} {
		a := NewAccessUnitAssembler(avframe.CodecH264, &H264Depacketizer{})
		for i := 0; i < 16384; i++ {
			frames, err := a.Push(accessUnitPacket(uint16(i), 100, complete && i == 16383, 0x65, 0xaa))
			if err != nil {
				t.Fatalf("packet %d rejected within limit: %v", i, err)
			}
			if complete && i == 16383 && (len(frames) != 1 || len(frames[0].Payload) != 16384*6) {
				t.Fatalf("exact packet limit did not emit complete media: frames=%d", len(frames))
			}
		}
		if !complete {
			if frames, err := a.Push(accessUnitPacket(16384, 100, true, 0x65, 0xbb)); err == nil || len(frames) != 0 || a.payload != nil {
				t.Fatalf("packet above limit accepted: frames=%d error=%v", len(frames), err)
			}
		}
		requireAccessUnitRecovery(t, a, 16385, 200)
	}
	// Parameter-only packets may produce no frame; they still consume the
	// packet budget before the codec can inspect packet 16385.
	a := NewAccessUnitAssembler(avframe.CodecH264, &H264Depacketizer{})
	for i := range 16384 {
		if _, err := a.Push(accessUnitPacket(uint16(i), 100, false, 0x67, 0x42, 0xc0, 0x0d)); err != nil {
			t.Fatal(err)
		}
	}
	if frames, err := a.Push(accessUnitPacket(16384, 100, true, 0x68, 0xcb, 0x83)); err == nil || len(frames) != 0 {
		t.Fatalf("parameter-only packets bypassed limit: frames=%v error=%v", frames, err)
	}
}

func TestAccessUnitBoundsPendingAV1AndVP9CodecBytes(t *testing.T) {
	for _, codec := range []avframe.CodecType{avframe.CodecAV1, avframe.CodecVP9} {
		dp, err := NewDepacketizer(codec)
		if err != nil {
			t.Fatal(err)
		}
		a := NewAccessUnitAssembler(codec, dp)
		payload := bytes.Repeat([]byte{0xaa}, (1<<20)+1)
		for i := 0; i < 17; i++ {
			if codec == avframe.CodecAV1 {
				payload[0] = 0x80
				if i == 0 {
					payload[0] = 0
				}
			} else {
				payload[0] = 0
				if i == 0 {
					payload[0] = 0x08
				}
			}
			frames, err := a.Push(accessUnitPacket(uint16(i), 100, false, payload...))
			if len(frames) != 0 || (i < 16 && err != nil) || (i == 16 && err == nil) {
				t.Fatalf("codec %v exceeded pending bytes: packet=%d frames=%d error=%v", codec, i, len(frames), err)
			}
			var pending []byte
			switch current := a.dp.(type) {
			case *AV1Depacketizer:
				pending = current.buf
			case *VP9Depacketizer:
				pending = current.buf
			}
			if cap(pending) > (16<<20)+4 {
				t.Fatalf("codec %v retained capacity %d above the byte ceiling", codec, cap(pending))
			}
		}
		if a.codecPending() || a.payload != nil {
			t.Fatalf("codec %v retained oversized pending state", codec)
		}
	}
}

func TestAccessUnitWraparoundAndFragmentedMedia(t *testing.T) {
	a := NewAccessUnitAssembler(avframe.CodecH264, &H264Depacketizer{})
	for i, packet := range []*pionrtp.Packet{
		accessUnitPacket(65534, 0xfffffff0, false, 0x65, 0x11),
		accessUnitPacket(65535, 0xfffffff0, false, 0x7c, 0x85, 0xaa),
		accessUnitPacket(0, 0xfffffff0, true, 0x7c, 0x45, 0xbb),
	} {
		frames, err := a.Push(packet)
		if err != nil {
			t.Fatal(err)
		}
		if i < 2 && len(frames) != 0 {
			t.Fatal("emitted incomplete fragmented access unit")
		}
		if i == 2 && (len(frames) != 1 || !bytes.Equal(frames[0].Payload, []byte{0, 0, 0, 2, 0x65, 0x11, 0, 0, 0, 3, 0x65, 0xaa, 0xbb})) {
			t.Fatalf("incorrect wrapped frame: frames=%v", frames)
		}
	}
	requireAccessUnitRecovery(t, a, 1, 20)
}

func FuzzAccessUnitAssembler(f *testing.F) {
	f.Add(uint8(0), []byte{2, 0x65, 0xaa, 0x83, 0x65, 0xbb})
	f.Add(uint8(0), []byte{3, 0x7c, 0x85, 0xaa, 3, 0x7c, 0x45, 0xbb})
	f.Add(uint8(4), []byte{2, 0, 0xaa, 2, 0x80, 0xbb})
	f.Fuzz(func(t *testing.T, codecIndex uint8, data []byte) {
		if len(data) > 2048 {
			t.Skip()
		}
		codecs := [...]avframe.CodecType{avframe.CodecH264, avframe.CodecH265, avframe.CodecVP8, avframe.CodecVP9, avframe.CodecAV1}
		codec := codecs[int(codecIndex)%len(codecs)]
		dp, err := NewDepacketizer(codec)
		if err != nil {
			t.Fatal(err)
		}
		a := NewAccessUnitAssembler(codec, dp)
		var sequence uint16
		timestamp := uint32(100)
		for offset := 0; offset < len(data); {
			control := data[offset]
			offset++
			size := min(int(control&31), len(data)-offset)
			sequence++
			if control&0x40 != 0 {
				sequence++
			}
			if control&0x80 != 0 {
				timestamp += 3000
			}
			frames, err := a.Push(accessUnitPacket(sequence, timestamp, control&0x20 != 0, data[offset:offset+size]...))
			offset += size
			if err != nil && len(frames) != 0 {
				t.Fatal("rejected packet emitted partial output")
			}
			if cap(a.payload) > (16<<20)+4 || a.packets > 16384 || (a.damaged && a.payload != nil) {
				t.Fatal("access unit retained excess or damaged state")
			}
			for _, frame := range frames {
				if len(frame.Payload) > (16<<20)+4 || cap(frame.Payload) > (16<<20)+4 {
					t.Fatal("access unit emitted unbounded output")
				}
			}
		}
	})
}
