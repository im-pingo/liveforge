package rtp

import (
	"bytes"
	"testing"

	"github.com/im-pingo/liveforge/pkg/avframe"
	pionrtp "github.com/pion/rtp/v2"
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
	for i, pkt := range pkts {
		pkt.SequenceNumber = uint16(i)
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

func TestVP8ExtendedDescriptorAndFreshFrameStart(t *testing.T) {
	d := &VP8Depacketizer{}
	for _, payload := range [][]byte{{0x00, 0, 9}, {0x11, 0, 9}} {
		if frame, err := d.Depacketize(&pionrtp.Packet{Header: pionrtp.Header{Marker: true}, Payload: payload}); frame != nil || err == nil {
			t.Fatalf("accepted continuation without frame start: frame=%v err=%v", frame, err)
		}
	}
	payload := []byte{0x90, 0xf0, 0x81, 0x23, 0x02, 0xa1, 0x00, 0x55}
	frame, err := d.Depacketize(&pionrtp.Packet{Header: pionrtp.Header{Marker: true}, Payload: payload})
	if err != nil || frame == nil || !bytes.Equal(frame.Payload, []byte{0, 0x55}) || frame.FrameType != avframe.FrameTypeKeyframe {
		t.Fatalf("extended descriptor leaked into media: frame=%+v err=%v", frame, err)
	}
	for n := 0; n < 7; n++ {
		if _, err := d.Depacketize(&pionrtp.Packet{Payload: payload[:n]}); err == nil {
			t.Fatalf("truncated descriptor %d accepted", n)
		}
	}
}

func TestVP8ContinuityBoundsAndRecovery(t *testing.T) {
	for _, kind := range []string{"sequence", "timestamp", "ssrc", "payload_type", "bytes", "fragments"} {
		t.Run(kind, func(t *testing.T) {
			d := &VP8Depacketizer{}
			start := &pionrtp.Packet{Header: pionrtp.Header{SequenceNumber: 1, Timestamp: 20, SSRC: 1, PayloadType: 96}, Payload: []byte{0x10, 0, 1}}
			if _, err := d.Depacketize(start); err != nil {
				t.Fatal(err)
			}
			continuation := &pionrtp.Packet{Header: pionrtp.Header{SequenceNumber: 2, Timestamp: 20, SSRC: 1, PayloadType: 96, Marker: true}, Payload: []byte{0, 2}}
			switch kind {
			case "sequence":
				continuation.SequenceNumber = 3
			case "timestamp":
				continuation.Timestamp++
			case "ssrc":
				continuation.SSRC++
			case "payload_type":
				continuation.PayloadType++
			case "bytes":
				continuation.Payload = make([]byte, (16<<20)+2)
			case "fragments":
				continuation.Marker = false
				for i := 2; i <= 16384; i++ {
					continuation.SequenceNumber = uint16(i)
					if _, err := d.Depacketize(continuation); err != nil {
						t.Fatalf("premature fragment bound at %d: %v", i, err)
					}
				}
				continuation.SequenceNumber++
			}
			if frame, err := d.Depacketize(continuation); err == nil || frame != nil {
				t.Fatalf("unbounded/discontinuous frame accepted=%v err=%v", frame != nil, err)
			}
			if d.buf != nil {
				t.Fatal("rejected frame retained payload")
			}
			start.Marker = true
			if frame, err := d.Depacketize(start); err != nil || frame == nil || !bytes.Equal(frame.Payload, []byte{0, 1}) {
				t.Fatalf("fresh frame did not recover: %+v %v", frame, err)
			}
		})
	}
}

func FuzzVP8Descriptor(f *testing.F) {
	for _, seed := range [][]byte{{0x10, 0, 1}, {0x90, 0xf0, 0x81, 0x23, 2, 0xa1, 0, 0x55}, {0x90, 0x80, 0x80}, {0, 0, 0}} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, payload []byte) {
		d := &VP8Depacketizer{}
		_, _ = d.Depacketize(&pionrtp.Packet{Header: pionrtp.Header{Marker: true}, Payload: payload})
	})
}

func TestVP8KeyframeDetection(t *testing.T) {
	// VP8 keyframe: bit 0 of first byte is 0.
	// See RFC 6386 §9.1: frame_tag = (frame_type << 0) | ...
	// frame_type=0 → keyframe, frame_type=1 → interframe.
	keyData := []byte{0x90, 0x01, 0x02}   // bit 0 = 0 → keyframe
	interData := []byte{0x91, 0x01, 0x02} // bit 0 = 1 → interframe

	tests := []struct {
		name string
		data []byte
		want avframe.FrameType
	}{
		{"keyframe", keyData, avframe.FrameTypeKeyframe},
		{"interframe", interData, avframe.FrameTypeInterframe},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			frame := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecVP8, avframe.FrameTypeKeyframe, 0, 0, tt.data)
			p := &VP8Packetizer{}
			pkts, err := p.Packetize(frame, 1400)
			if err != nil {
				t.Fatal(err)
			}
			d := &VP8Depacketizer{}
			var result *avframe.AVFrame
			for _, pkt := range pkts {
				f, _ := d.Depacketize(pkt)
				if f != nil {
					result = f
				}
			}
			if result == nil {
				t.Fatal("no frame")
			}
			if result.FrameType != tt.want {
				t.Errorf("got %v, want %v", result.FrameType, tt.want)
			}
		})
	}
}
