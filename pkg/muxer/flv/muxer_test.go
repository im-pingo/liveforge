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

func TestMuxEnhancedVideoH265(t *testing.T) {
	var buf bytes.Buffer
	m := NewMuxer()

	frame := avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH265, avframe.FrameTypeKeyframe,
		0, 0, []byte{0x26, 0x01, 0x02, 0x03},
	)

	if err := m.WriteFrame(&buf, frame); err != nil {
		t.Fatalf("WriteFrame error: %v", err)
	}

	data := buf.Bytes()
	if data[0] != TagTypeVideo {
		t.Errorf("tag type = %d, want %d", data[0], TagTypeVideo)
	}

	// Enhanced video: first data byte should have 0x80 set
	videoByte := data[TagHeaderSize]
	if videoByte&0x80 == 0 {
		t.Errorf("enhanced flag not set: 0x%02X", videoByte)
	}

	// FourCC should be "hvc1"
	fourcc := string(data[TagHeaderSize+1 : TagHeaderSize+5])
	if fourcc != "hvc1" {
		t.Errorf("FourCC = %s, want hvc1", fourcc)
	}
}

func TestMuxEnhancedVideoAV1(t *testing.T) {
	var buf bytes.Buffer
	m := NewMuxer()

	frame := avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecAV1, avframe.FrameTypeKeyframe,
		100, 100, []byte{0x0A, 0x05, 0x00},
	)

	if err := m.WriteFrame(&buf, frame); err != nil {
		t.Fatalf("WriteFrame error: %v", err)
	}

	data := buf.Bytes()
	fourcc := string(data[TagHeaderSize+1 : TagHeaderSize+5])
	if fourcc != "av01" {
		t.Errorf("FourCC = %s, want av01", fourcc)
	}
}

func TestMuxEnhancedVideoSequenceHeader(t *testing.T) {
	var buf bytes.Buffer
	m := NewMuxer()

	frame := avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH265, avframe.FrameTypeSequenceHeader,
		0, 0, []byte{0x01, 0x01, 0x60},
	)

	if err := m.WriteFrame(&buf, frame); err != nil {
		t.Fatalf("WriteFrame error: %v", err)
	}

	data := buf.Bytes()
	videoByte := data[TagHeaderSize]
	// Packet type should be 0 (SequenceStart) in lower nibble
	packetType := videoByte & 0x0F
	if packetType != ExVideoPacketSequenceStart {
		t.Errorf("packet type = %d, want %d", packetType, ExVideoPacketSequenceStart)
	}
}

func TestMuxEnhancedAudioOpus(t *testing.T) {
	var buf bytes.Buffer
	m := NewMuxer()

	frame := avframe.NewAVFrame(
		avframe.MediaTypeAudio, avframe.CodecOpus, avframe.FrameTypeInterframe,
		100, 100, []byte{0xFC, 0x01, 0x02},
	)

	if err := m.WriteFrame(&buf, frame); err != nil {
		t.Fatalf("WriteFrame error: %v", err)
	}

	data := buf.Bytes()
	if data[0] != TagTypeAudio {
		t.Errorf("tag type = %d, want %d", data[0], TagTypeAudio)
	}

	// Enhanced audio: first data byte should be 0x90
	audioByte := data[TagHeaderSize]
	if audioByte != 0x90 {
		t.Errorf("enhanced audio byte = 0x%02X, want 0x90", audioByte)
	}

	// FourCC should be "Opus"
	fourcc := string(data[TagHeaderSize+1 : TagHeaderSize+5])
	if fourcc != "Opus" {
		t.Errorf("FourCC = %s, want Opus", fourcc)
	}
}

func TestMuxMP3Audio(t *testing.T) {
	var buf bytes.Buffer
	m := NewMuxer()

	frame := avframe.NewAVFrame(
		avframe.MediaTypeAudio, avframe.CodecMP3, avframe.FrameTypeInterframe,
		0, 0, []byte{0xFF, 0xFB, 0x90, 0x00},
	)

	if err := m.WriteFrame(&buf, frame); err != nil {
		t.Fatalf("WriteFrame error: %v", err)
	}

	data := buf.Bytes()
	// MP3 format ID = 2, upper nibble of first audio byte
	audioFormat := data[TagHeaderSize] >> 4
	if audioFormat != AudioFormatMP3 {
		t.Errorf("audio format = %d, want %d", audioFormat, AudioFormatMP3)
	}
}
