package integration

import (
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/audiocodec"
	"github.com/im-pingo/liveforge/pkg/avframe"
)

func TestTranscodeAVSync(t *testing.T) {
	cfg := config.StreamConfig{RingBufferSize: 256}
	bus := core.NewEventBus()
	s := core.NewStream("avsync-test", cfg, config.LimitsConfig{}, bus)
	core.SetTranscodeManagerForTest(s, core.NewTranscodeManager(s, audiocodec.Global(), 256))

	pub := &mockPublisher{info: &avframe.MediaInfo{
		AudioCodec: avframe.CodecG711U,
		VideoCodec: avframe.CodecH264,
	}}
	_ = s.SetPublisher(pub)

	reader, release, err := s.TranscodeManager().GetOrCreateReader(avframe.CodecG711A)
	if err != nil {
		t.Fatalf("GetOrCreateReader: %v", err)
	}
	defer release()

	go func() {
		for i := 0; i < 100; i++ {
			vDTS := int64(i * 20)
			s.WriteFrame(avframe.NewAVFrame(
				avframe.MediaTypeVideo, avframe.CodecH264,
				avframe.FrameTypeInterframe, vDTS, vDTS,
				[]byte{0x00, 0x01, 0x02},
			))

			aDTS := int64(i * 20)
			// G.711U: 160 bytes = 20ms at 8kHz
			s.WriteFrame(avframe.NewAVFrame(
				avframe.MediaTypeAudio, avframe.CodecG711U,
				avframe.FrameTypeInterframe, aDTS, aDTS,
				make([]byte, 160),
			))
			time.Sleep(time.Millisecond)
		}
	}()

	var lastVideoDTS, lastAudioDTS int64
	videoCount, audioCount := 0, 0
	timeout := time.After(3 * time.Second)

	for videoCount < 10 && audioCount < 10 {
		select {
		case <-timeout:
			t.Fatalf("timeout: got %d video, %d audio frames", videoCount, audioCount)
		default:
		}

		frame, ok := reader.TryRead()
		if !ok {
			time.Sleep(time.Millisecond)
			continue
		}

		if frame.MediaType.IsVideo() {
			lastVideoDTS = frame.DTS
			videoCount++
		} else if frame.MediaType.IsAudio() {
			lastAudioDTS = frame.DTS
			audioCount++
		}
	}

	drift := lastVideoDTS - lastAudioDTS
	if drift < 0 {
		drift = -drift
	}
	if drift > 50 {
		t.Errorf("A/V drift too large: video DTS=%d, audio DTS=%d, drift=%dms",
			lastVideoDTS, lastAudioDTS, drift)
	}
}

type mockPublisher struct {
	info *avframe.MediaInfo
}

func (p *mockPublisher) ID() string                    { return "mock-pub" }
func (p *mockPublisher) MediaInfo() *avframe.MediaInfo { return p.info }
func (p *mockPublisher) Close() error                  { return nil }
