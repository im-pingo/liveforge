package flv

import (
	"bytes"
	"testing"

	"github.com/im-pingo/liveforge/pkg/avframe"
)

func TestMuxWriteHeader(t *testing.T) {
	var buf bytes.Buffer
	m := NewMuxer()
	if err := m.WriteHeader(&buf, true, true); err != nil {
		t.Fatalf("WriteHeader error: %v", err)
	}
	// FLV header (9) + PreviousTagSize0 (4)
	if buf.Len() != 13 {
		t.Errorf("expected 13 bytes, got %d", buf.Len())
	}
	// Check FLV signature
	if buf.Bytes()[0] != 'F' || buf.Bytes()[1] != 'L' || buf.Bytes()[2] != 'V' {
		t.Error("invalid FLV signature")
	}
	// Check flags byte: audio+video = 0x05
	if buf.Bytes()[4] != 0x05 {
		t.Errorf("expected flags 0x05, got 0x%02x", buf.Bytes()[4])
	}
}

func TestMuxVideoFrame(t *testing.T) {
	var buf bytes.Buffer
	m := NewMuxer()

	frame := avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe,
		1000, 1000, []byte{0x65, 0x88, 0x00, 0x01},
	)

	if err := m.WriteFrame(&buf, frame); err != nil {
		t.Fatalf("WriteFrame error: %v", err)
	}

	data := buf.Bytes()
	// Should have tag header (11) + video header (5) + payload (4) + prev tag size (4)
	expectedLen := 11 + 5 + 4 + 4
	if len(data) != expectedLen {
		t.Errorf("expected %d bytes, got %d", expectedLen, len(data))
	}

	// Verify tag type
	if data[0] != TagTypeVideo {
		t.Errorf("expected video tag type %d, got %d", TagTypeVideo, data[0])
	}
}

func TestMuxRoundTrip(t *testing.T) {
	// Mux → Demux round trip
	var buf bytes.Buffer
	m := NewMuxer()

	if err := m.WriteHeader(&buf, true, true); err != nil {
		t.Fatal(err)
	}

	frames := []*avframe.AVFrame{
		avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader, 0, 0, []byte{0x01, 0x64, 0x00, 0x28}),
		avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeSequenceHeader, 0, 0, []byte{0x12, 0x10}),
		avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 0, 0, []byte{0x65, 0x88}),
		avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe, 23, 23, []byte{0xFF, 0x01}),
		avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe, 40, 40, []byte{0x41, 0x01}),
	}

	for _, f := range frames {
		if err := m.WriteFrame(&buf, f); err != nil {
			t.Fatalf("WriteFrame error: %v", err)
		}
	}

	// Demux
	reader := bytes.NewReader(buf.Bytes())
	d := NewDemuxer(reader)

	for i, expected := range frames {
		got, err := d.ReadTag()
		if err != nil {
			t.Fatalf("ReadTag[%d] error: %v", i, err)
		}
		if got.MediaType != expected.MediaType {
			t.Errorf("[%d] media type: got %v, want %v", i, got.MediaType, expected.MediaType)
		}
		if got.Codec != expected.Codec {
			t.Errorf("[%d] codec: got %v, want %v", i, got.Codec, expected.Codec)
		}
		if got.FrameType != expected.FrameType {
			t.Errorf("[%d] frame type: got %v, want %v", i, got.FrameType, expected.FrameType)
		}
		if got.DTS != expected.DTS {
			t.Errorf("[%d] DTS: got %d, want %d", i, got.DTS, expected.DTS)
		}
		if !bytes.Equal(got.Payload, expected.Payload) {
			t.Errorf("[%d] payload mismatch", i)
		}
	}
}
