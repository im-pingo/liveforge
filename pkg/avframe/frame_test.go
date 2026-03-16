package avframe

import (
	"testing"
)

func TestNewAVFrame(t *testing.T) {
	payload := []byte{0x00, 0x01, 0x02}
	f := NewAVFrame(MediaTypeVideo, CodecH264, FrameTypeKeyframe, 1000, 1000, payload)

	if f.MediaType != MediaTypeVideo {
		t.Errorf("expected MediaTypeVideo, got %v", f.MediaType)
	}
	if f.Codec != CodecH264 {
		t.Errorf("expected CodecH264, got %v", f.Codec)
	}
	if f.FrameType != FrameTypeKeyframe {
		t.Errorf("expected FrameTypeKeyframe, got %v", f.FrameType)
	}
	if f.DTS != 1000 {
		t.Errorf("expected DTS 1000, got %d", f.DTS)
	}
	if f.PTS != 1000 {
		t.Errorf("expected PTS 1000, got %d", f.PTS)
	}
	if len(f.Payload) != 3 {
		t.Errorf("expected payload len 3, got %d", len(f.Payload))
	}
}

func TestCodecTypeString(t *testing.T) {
	tests := []struct {
		codec CodecType
		want  string
	}{
		{CodecH264, "H264"},
		{CodecH265, "H265"},
		{CodecAV1, "AV1"},
		{CodecVP8, "VP8"},
		{CodecVP9, "VP9"},
		{CodecAAC, "AAC"},
		{CodecOpus, "Opus"},
		{CodecMP3, "MP3"},
		{CodecG711A, "G711A"},
		{CodecG711U, "G711U"},
		{CodecG722, "G722"},
		{CodecG729, "G729"},
		{CodecSpeex, "Speex"},
	}
	for _, tt := range tests {
		if got := tt.codec.String(); got != tt.want {
			t.Errorf("CodecType(%d).String() = %q, want %q", tt.codec, got, tt.want)
		}
	}
}

func TestMediaTypeIsVideo(t *testing.T) {
	if !MediaTypeVideo.IsVideo() {
		t.Error("MediaTypeVideo.IsVideo() should be true")
	}
	if MediaTypeAudio.IsVideo() {
		t.Error("MediaTypeAudio.IsVideo() should be false")
	}
}

func TestFrameTypeIsKeyframe(t *testing.T) {
	if !FrameTypeKeyframe.IsKeyframe() {
		t.Error("FrameTypeKeyframe.IsKeyframe() should be true")
	}
	if FrameTypeInterframe.IsKeyframe() {
		t.Error("FrameTypeInterframe.IsKeyframe() should be false")
	}
}

func TestCodecTypeIsVideo(t *testing.T) {
	videoCodecs := []CodecType{CodecH264, CodecH265, CodecAV1, CodecVP8, CodecVP9}
	for _, c := range videoCodecs {
		if !c.IsVideo() {
			t.Errorf("%s.IsVideo() should be true", c)
		}
		if c.IsAudio() {
			t.Errorf("%s.IsAudio() should be false", c)
		}
	}

	audioCodecs := []CodecType{CodecAAC, CodecOpus, CodecMP3, CodecG711A, CodecG711U, CodecG722, CodecG729, CodecSpeex}
	for _, c := range audioCodecs {
		if !c.IsAudio() {
			t.Errorf("%s.IsAudio() should be true", c)
		}
		if c.IsVideo() {
			t.Errorf("%s.IsVideo() should be false", c)
		}
	}
}
