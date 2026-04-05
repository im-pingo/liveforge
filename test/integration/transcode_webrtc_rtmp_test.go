package integration

import (
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/audiocodec"
	"github.com/im-pingo/liveforge/pkg/avframe"
)

func TestWebRTCtoRTMP(t *testing.T) {
	cfg := config.StreamConfig{RingBufferSize: 256}
	bus := core.NewEventBus()
	s := core.NewStream("webrtc-rtmp", cfg, config.LimitsConfig{}, bus)
	core.SetTranscodeManagerForTest(s, core.NewTranscodeManager(s, audiocodec.Global(), 256))

	pub := &mockPublisher{info: &avframe.MediaInfo{
		AudioCodec: avframe.CodecOpus,
	}}
	_ = s.SetPublisher(pub)

	reader, release, err := s.TranscodeManager().GetOrCreateReader(avframe.CodecAAC)
	if err != nil {
		t.Fatalf("GetOrCreateReader(AAC): %v", err)
	}
	defer release()

	enc := audiocodec.NewFFmpegEncoder("libopus", 48000, 2)
	defer enc.Close()
	silence := &audiocodec.PCMFrame{
		Samples:    make([]int16, 960*2),
		SampleRate: 48000,
		Channels:   2,
	}
	opusPayload, err := enc.Encode(silence)
	if err != nil {
		t.Fatalf("encode Opus: %v", err)
	}

	// Write frames from a goroutine so the transcodeLoop has time to start
	go func() {
		for i := 0; i < 20; i++ {
			dts := int64(i) * 20
			s.WriteFrame(avframe.NewAVFrame(
				avframe.MediaTypeAudio, avframe.CodecOpus,
				avframe.FrameTypeInterframe, dts, dts,
				opusPayload,
			))
			time.Sleep(time.Millisecond)
		}
	}()

	aacCount := 0
	gotSeqHeader := false
	timeout := time.After(3 * time.Second)
	for aacCount < 5 {
		select {
		case <-timeout:
			t.Fatalf("timeout: got %d AAC frames, seqHeader=%v", aacCount, gotSeqHeader)
		default:
		}
		frame, ok := reader.TryRead()
		if !ok {
			time.Sleep(time.Millisecond)
			continue
		}
		if frame.MediaType.IsAudio() {
			if frame.Codec != avframe.CodecAAC {
				t.Fatalf("expected AAC, got codec %d", frame.Codec)
			}
			if frame.FrameType == avframe.FrameTypeSequenceHeader {
				gotSeqHeader = true
				continue
			}
			aacCount++
		}
	}
	if !gotSeqHeader {
		t.Error("expected AAC sequence header before audio frames")
	}
	t.Logf("received %d AAC frames from Opus source", aacCount)
}
