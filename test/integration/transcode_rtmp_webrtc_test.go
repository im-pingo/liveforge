package integration

import (
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/audiocodec"
	"github.com/im-pingo/liveforge/pkg/avframe"
)

func TestRTMPtoWebRTC(t *testing.T) {
	cfg := config.StreamConfig{RingBufferSize: 256}
	bus := core.NewEventBus()
	s := core.NewStream("rtmp-webrtc", cfg, config.LimitsConfig{}, bus)
	core.SetTranscodeManagerForTest(s, core.NewTranscodeManager(s, audiocodec.Global(), 256))

	pub := &mockPublisher{info: &avframe.MediaInfo{
		AudioCodec: avframe.CodecAAC,
	}}
	_ = s.SetPublisher(pub)

	// Write sequence header before creating transcoder so AAC extradata is cached
	s.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeAudio, avframe.CodecAAC,
		avframe.FrameTypeSequenceHeader, 0, 0,
		[]byte{0x12, 0x10},
	))

	reader, release, err := s.TranscodeManager().GetOrCreateReader(avframe.CodecOpus)
	if err != nil {
		t.Fatalf("GetOrCreateReader(Opus): %v", err)
	}
	defer release()

	enc := audiocodec.NewFFmpegEncoder("aac", 44100, 2)
	defer enc.Close()
	silence := &audiocodec.PCMFrame{
		Samples:    make([]int16, 1024*2),
		SampleRate: 44100,
		Channels:   2,
	}
	aacPayload, err := enc.Encode(silence)
	if err != nil {
		t.Fatalf("encode AAC: %v", err)
	}

	// Write frames from a goroutine so the transcodeLoop has time to start
	go func() {
		for i := 0; i < 10; i++ {
			dts := int64(i) * 23
			s.WriteFrame(avframe.NewAVFrame(
				avframe.MediaTypeAudio, avframe.CodecAAC,
				avframe.FrameTypeInterframe, dts, dts,
				aacPayload,
			))
			time.Sleep(time.Millisecond)
		}
	}()

	opusCount := 0
	timeout := time.After(3 * time.Second)
	for opusCount < 5 {
		select {
		case <-timeout:
			t.Fatalf("timeout: only got %d Opus frames", opusCount)
		default:
		}
		frame, ok := reader.TryRead()
		if !ok {
			time.Sleep(time.Millisecond)
			continue
		}
		if frame.FrameType == avframe.FrameTypeSequenceHeader {
			continue
		}
		if frame.MediaType.IsAudio() {
			if frame.Codec != avframe.CodecOpus {
				t.Fatalf("expected Opus, got codec %d", frame.Codec)
			}
			opusCount++
		}
	}
	t.Logf("received %d Opus frames from AAC source", opusCount)
}
