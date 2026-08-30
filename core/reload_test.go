package core

import (
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/pkg/avframe"
)

func TestStreamHubUpdatePolicyPublishesHotValues(t *testing.T) {
	bus := NewEventBus()
	hub := NewStreamHub(config.StreamConfig{GOPCacheNum: 1, Feedback: config.FeedbackConfig{DefaultMode: "drop"}}, config.LimitsConfig{MaxConnections: 10}, bus)
	stream, err := hub.GetOrCreate("live/reload")
	if err != nil {
		t.Fatal(err)
	}

	nextStream := stream.Config()
	nextStream.GOPCacheNum = 4
	nextStream.NoPublisherTimeout = 3 * time.Second
	nextStream.Feedback.DefaultMode = "passthrough"
	nextLimits := config.LimitsConfig{MaxConnections: 20}
	hub.UpdatePolicy(nextStream, nextLimits)

	if got := stream.Config().GOPCacheNum; got != 4 {
		t.Fatalf("GOPCacheNum = %d, want 4", got)
	}
	if got := stream.Config().NoPublisherTimeout; got != 3*time.Second {
		t.Fatalf("NoPublisherTimeout = %v, want 3s", got)
	}
	if got := stream.FeedbackRouter().EffectiveMode(); got != FeedbackPassthrough {
		t.Fatalf("feedback mode = %v, want passthrough", got)
	}
	if got := hub.Limits().MaxConnections; got != 20 {
		t.Fatalf("MaxConnections = %d, want 20", got)
	}
}

func TestStreamUpdatePolicyReconcilesGOPCacheAndTimeouts(t *testing.T) {
	cfg := config.StreamConfig{
		GOPCache:           true,
		GOPCacheNum:        2,
		RingBufferSize:     16,
		NoPublisherTimeout: time.Hour,
	}
	stream := NewStream("live/policy", cfg, config.LimitsConfig{}, NewEventBus())
	stream.WriteFrame(&avframe.AVFrame{MediaType: avframe.MediaTypeVideo, FrameType: avframe.FrameTypeKeyframe, DTS: 0})
	stream.WriteFrame(&avframe.AVFrame{MediaType: avframe.MediaTypeAudio, FrameType: avframe.FrameTypeInterframe, DTS: 1})
	if stream.GOPCacheLen() == 0 {
		t.Fatal("test setup did not populate the GOP cache")
	}
	stream.RemovePublisher()

	next := cfg
	next.GOPCache = false
	next.NoPublisherTimeout = 10 * time.Millisecond
	stream.UpdatePolicy(next, config.LimitsConfig{})
	if stream.GOPCacheLen() != 0 {
		t.Fatalf("disabled GOP cache retained %d frames", stream.GOPCacheLen())
	}
	deadline := time.Now().Add(time.Second)
	for stream.State() != StreamStateDestroying && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if stream.State() != StreamStateDestroying {
		t.Fatal("updated no-publisher timeout did not replace the active timer")
	}
}

func TestStreamUpdatePolicyRelaxationReopensCurrentGOPForFutureFrames(t *testing.T) {
	tests := []struct {
		name    string
		tighten func(*config.StreamConfig)
		relax   func(*config.StreamConfig)
	}{
		{
			name: "frames",
			tighten: func(cfg *config.StreamConfig) {
				cfg.GOPCacheMaxFrames = 2
			},
			relax: func(cfg *config.StreamConfig) {
				cfg.GOPCacheMaxFrames = 4
			},
		},
		{
			name: "bytes",
			tighten: func(cfg *config.StreamConfig) {
				cfg.GOPCacheMaxBytes = 4
			},
			relax: func(cfg *config.StreamConfig) {
				cfg.GOPCacheMaxBytes = 7
			},
		},
		{
			name: "duration",
			tighten: func(cfg *config.StreamConfig) {
				cfg.GOPCacheMaxDuration = 20 * time.Millisecond
			},
			relax: func(cfg *config.StreamConfig) {
				cfg.GOPCacheMaxDuration = 60 * time.Millisecond
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := newTestStreamConfig()
			stream := NewStream("live/policy-relax-"+test.name, cfg, config.LimitsConfig{}, NewEventBus())
			publisher := &testPublisher{
				id:   "publisher-" + test.name,
				info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264, AudioCodec: avframe.CodecAAC},
			}
			if err := stream.SetPublisher(publisher); err != nil {
				t.Fatal(err)
			}

			keyframe := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 0, 0, []byte{1, 2})
			initialAudio := avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe, 20, 20, []byte{3, 4})
			omittedVideo := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe, 40, 40, []byte{5})
			futureAudio := avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe, 40, 40, []byte{6})
			futureVideo := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe, 60, 60, []byte{7, 8})
			afterRelaxedBound := avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe, 80, 80, []byte{9})
			assertGOP := func(stage string, want ...*avframe.AVFrame) {
				t.Helper()
				got := stream.GOPCache()
				if len(got) != len(want) {
					t.Fatalf("%s GOP length = %d, want %d", stage, len(got), len(want))
				}
				for i := range want {
					if got[i] != want[i] {
						t.Fatalf("%s GOP frame[%d] = %p, want %p", stage, i, got[i], want[i])
					}
				}
			}

			stream.WriteFrame(keyframe)
			stream.WriteFrame(initialAudio)
			tightened := cfg
			test.tighten(&tightened)
			stream.UpdatePolicy(tightened, config.LimitsConfig{})
			stream.WriteFrame(omittedVideo)
			assertGOP("tightened and sealed", keyframe, initialAudio)

			relaxed := cfg
			test.relax(&relaxed)
			stream.UpdatePolicy(relaxed, config.LimitsConfig{})
			assertGOP("relaxed before future frames", keyframe, initialAudio)
			stream.WriteFrame(futureAudio)
			stream.WriteFrame(futureVideo)
			stream.WriteFrame(afterRelaxedBound)
			assertGOP("relaxed at new bound", keyframe, initialAudio, futureAudio, futureVideo)
		})
	}
}

func TestStreamUpdatePolicyCombinesGOPBoundsAndRetainsOnlyPlayablePrefix(t *testing.T) {
	cfg := newTestStreamConfig()
	stream := NewStream("live/policy-combined-bounds", cfg, config.LimitsConfig{}, NewEventBus())

	keyframe := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 0, 0, []byte{1, 2})
	initialAudio := avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe, 20, 20, []byte{3, 4})
	omittedVideo := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe, 40, 40, []byte{5, 6, 7})
	omittedAudio := avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe, 60, 60, []byte{8})
	for _, frame := range []*avframe.AVFrame{keyframe, initialAudio, omittedVideo, omittedAudio} {
		stream.WriteFrame(frame)
	}

	tightened := cfg
	tightened.GOPCacheMaxFrames = 4
	tightened.GOPCacheMaxBytes = 6
	tightened.GOPCacheMaxDuration = 60 * time.Millisecond
	stream.UpdatePolicy(tightened, config.LimitsConfig{})
	assertCachedFrames(t, stream, "combined bounds use shortest playable prefix", keyframe, initialAudio)

	relaxed := tightened
	relaxed.GOPCacheMaxBytes = 8
	stream.UpdatePolicy(relaxed, config.LimitsConfig{})
	futureVideo := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe, 40, 40, []byte{9, 10, 11})
	futureAudio := avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe, 60, 60, []byte{12})
	afterAllBounds := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe, 80, 80, []byte{13})
	stream.WriteFrame(futureVideo)
	stream.WriteFrame(futureAudio)
	stream.WriteFrame(afterAllBounds)
	assertCachedFrames(t, stream, "relaxed byte bound preserves all remaining bounds", keyframe, initialAudio, futureVideo, futureAudio)
}

func TestStreamUpdatePolicyRelaxationReopensOnlyActiveRetainedGOP(t *testing.T) {
	cfg := newTestStreamConfig()
	cfg.GOPCacheNum = 3
	stream := NewStream("live/policy-multiple-gops", cfg, config.LimitsConfig{}, NewEventBus())

	var retained []*avframe.AVFrame
	for gop := int64(0); gop < 3; gop++ {
		base := gop * 100
		keyframe := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, base, base, []byte{byte(gop + 1)})
		audio := avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe, base+20, base+20, []byte{byte(gop + 11)})
		video := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe, base+40, base+40, []byte{byte(gop + 21)})
		retained = append(retained, keyframe, audio)
		stream.WriteFrame(keyframe)
		stream.WriteFrame(audio)
		stream.WriteFrame(video)
	}

	tightened := cfg
	tightened.GOPCacheMaxFrames = 2
	stream.UpdatePolicy(tightened, config.LimitsConfig{})
	assertCachedFrames(t, stream, "all retained GOPs trimmed", retained...)

	relaxed := cfg
	relaxed.GOPCacheMaxFrames = 3
	stream.UpdatePolicy(relaxed, config.LimitsConfig{})
	futureActiveAudio := avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe, 260, 260, []byte{31})
	stream.WriteFrame(futureActiveAudio)
	retained = append(retained, futureActiveAudio)
	assertCachedFrames(t, stream, "only active GOP accepts future frame", retained...)
}

func TestStreamUpdatePolicyZeroDisablesOnlySelectedGOPBound(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*config.StreamConfig)
		zero      func(*config.StreamConfig)
	}{
		{
			name: "frames",
			configure: func(cfg *config.StreamConfig) {
				cfg.GOPCacheMaxFrames = 2
				cfg.GOPCacheMaxBytes = 7
			},
			zero: func(cfg *config.StreamConfig) { cfg.GOPCacheMaxFrames = 0 },
		},
		{
			name: "bytes",
			configure: func(cfg *config.StreamConfig) {
				cfg.GOPCacheMaxBytes = 4
				cfg.GOPCacheMaxFrames = 4
			},
			zero: func(cfg *config.StreamConfig) { cfg.GOPCacheMaxBytes = 0 },
		},
		{
			name: "duration",
			configure: func(cfg *config.StreamConfig) {
				cfg.GOPCacheMaxDuration = 20 * time.Millisecond
				cfg.GOPCacheMaxFrames = 4
			},
			zero: func(cfg *config.StreamConfig) { cfg.GOPCacheMaxDuration = 0 },
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := newTestStreamConfig()
			test.configure(&cfg)
			stream := NewStream("live/policy-zero-"+test.name, cfg, config.LimitsConfig{}, NewEventBus())
			keyframe := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 0, 0, []byte{1, 2})
			initialAudio := avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe, 20, 20, []byte{3, 4})
			stream.WriteFrame(keyframe)
			stream.WriteFrame(initialAudio)
			assertCachedFrames(t, stream, "selected nonzero bound seals active GOP", keyframe, initialAudio)

			reloaded := cfg
			test.zero(&reloaded)
			stream.UpdatePolicy(reloaded, config.LimitsConfig{})
			futureVideo := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe, 40, 40, []byte{5, 6})
			futureAudio := avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe, 60, 60, []byte{7})
			afterRemainingBound := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe, 80, 80, []byte{8})
			stream.WriteFrame(futureVideo)
			stream.WriteFrame(futureAudio)
			stream.WriteFrame(afterRemainingBound)
			assertCachedFrames(t, stream, "zero disables selected bound only", keyframe, initialAudio, futureVideo, futureAudio)
		})
	}
}

func assertCachedFrames(t *testing.T, stream *Stream, stage string, want ...*avframe.AVFrame) {
	t.Helper()
	got := stream.GOPCache()
	if len(got) != len(want) {
		t.Fatalf("%s: cached frames = %d, want %d", stage, len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: cached frame[%d] = %p, want %p", stage, i, got[i], want[i])
		}
	}
}
