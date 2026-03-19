package rtsp

import (
	"bytes"
	"testing"

	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
)

func TestRTSPSubscriberInterface(t *testing.T) {
	var _ core.Subscriber = (*RTSPSubscriber)(nil)
}

func TestRTSPSubscriberIDAndOptions(t *testing.T) {
	info := &avframe.MediaInfo{VideoCodec: avframe.CodecH264}
	sub, err := NewRTSPSubscriber("test-sub", info, &bytes.Buffer{}, 0, 2)
	if err != nil {
		t.Fatalf("NewRTSPSubscriber: %v", err)
	}
	if sub.ID() != "test-sub" {
		t.Errorf("ID = %q, want %q", sub.ID(), "test-sub")
	}
	if sub.Options().StartMode != core.StartModeGOP {
		t.Errorf("StartMode = %v, want %v", sub.Options().StartMode, core.StartModeGOP)
	}
}

func TestRTSPSubscriberSendFrame(t *testing.T) {
	info := &avframe.MediaInfo{VideoCodec: avframe.CodecH264}
	var buf bytes.Buffer
	sub, err := NewRTSPSubscriber("test-sub", info, &buf, 0, 2)
	if err != nil {
		t.Fatalf("NewRTSPSubscriber: %v", err)
	}
	// Create a small H.264 frame (single NAL that fits in one packet).
	nalData := make([]byte, 100)
	nalData[0] = 0x65 // IDR slice
	frame := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 1000, 1000, nalData)
	err = sub.SendFrame(frame)
	if err != nil {
		t.Fatalf("SendFrame: %v", err)
	}
	// Verify something was written (interleaved frame).
	if buf.Len() == 0 {
		t.Error("expected data to be written")
	}
	// First byte should be '$' (interleaved marker).
	if buf.Bytes()[0] != '$' {
		t.Errorf("first byte = 0x%02x, want '$'", buf.Bytes()[0])
	}
}

func TestRTSPSubscriberClose(t *testing.T) {
	info := &avframe.MediaInfo{VideoCodec: avframe.CodecH264}
	sub, err := NewRTSPSubscriber("test-sub", info, &bytes.Buffer{}, 0, 2)
	if err != nil {
		t.Fatalf("NewRTSPSubscriber: %v", err)
	}
	if err := sub.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// After close, SendFrame should silently return nil.
	frame := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 0, 0, make([]byte, 100))
	if err := sub.SendFrame(frame); err != nil {
		t.Fatalf("SendFrame after close: %v", err)
	}
	// Done channel should be closed.
	select {
	case <-sub.Done():
	default:
		t.Error("Done channel not closed after Close()")
	}
}

func TestRTSPSubscriberAudioVideo(t *testing.T) {
	info := &avframe.MediaInfo{
		VideoCodec: avframe.CodecH264,
		AudioCodec: avframe.CodecAAC,
		SampleRate: 44100,
	}
	var buf bytes.Buffer
	sub, err := NewRTSPSubscriber("test-sub", info, &buf, 0, 2)
	if err != nil {
		t.Fatalf("NewRTSPSubscriber: %v", err)
	}

	// Send video frame.
	videoData := make([]byte, 100)
	videoData[0] = 0x65 // IDR
	vFrame := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 0, 0, videoData)
	if err := sub.SendFrame(vFrame); err != nil {
		t.Fatalf("SendFrame video: %v", err)
	}
	videoLen := buf.Len()

	// Send audio frame (AAC AU).
	audioData := make([]byte, 400)
	aFrame := avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe, 0, 0, audioData)
	if err := sub.SendFrame(aFrame); err != nil {
		t.Fatalf("SendFrame audio: %v", err)
	}

	if buf.Len() <= videoLen {
		t.Error("expected more data after audio frame")
	}
}

func TestRTSPSubscriberOnDataNoop(t *testing.T) {
	info := &avframe.MediaInfo{VideoCodec: avframe.CodecH264}
	sub, err := NewRTSPSubscriber("test-sub", info, &bytes.Buffer{}, 0, 2)
	if err != nil {
		t.Fatalf("NewRTSPSubscriber: %v", err)
	}
	// OnData is a no-op for RTSP subscribers.
	if err := sub.OnData([]byte("test")); err != nil {
		t.Fatalf("OnData: %v", err)
	}
}

func TestCodecDefaultPT(t *testing.T) {
	tests := []struct {
		codec avframe.CodecType
		want  uint8
	}{
		{avframe.CodecH264, 96},
		{avframe.CodecH265, 97},
		{avframe.CodecAAC, 101},
		{avframe.CodecOpus, 111},
		{avframe.CodecG711U, 0},
		{avframe.CodecG711A, 8},
		{avframe.CodecMP3, 14},
	}
	for _, tt := range tests {
		got := codecDefaultPT(tt.codec)
		if got != tt.want {
			t.Errorf("codecDefaultPT(%v) = %d, want %d", tt.codec, got, tt.want)
		}
	}
}

func TestAudioClockRate(t *testing.T) {
	tests := []struct {
		info *avframe.MediaInfo
		want uint32
	}{
		{&avframe.MediaInfo{AudioCodec: avframe.CodecOpus}, 48000},
		{&avframe.MediaInfo{AudioCodec: avframe.CodecG711U}, 8000},
		{&avframe.MediaInfo{AudioCodec: avframe.CodecG711A}, 8000},
		{&avframe.MediaInfo{AudioCodec: avframe.CodecMP3}, 90000},
		{&avframe.MediaInfo{AudioCodec: avframe.CodecAAC, SampleRate: 48000}, 48000},
		{&avframe.MediaInfo{AudioCodec: avframe.CodecAAC}, 44100},
	}
	for _, tt := range tests {
		got := audioClockRate(tt.info)
		if got != tt.want {
			t.Errorf("audioClockRate(%v) = %d, want %d", tt.info.AudioCodec, got, tt.want)
		}
	}
}
