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

func TestStreamUpdatePolicyReconcilesCachesAndTimeouts(t *testing.T) {
	cfg := config.StreamConfig{
		GOPCache:           true,
		GOPCacheNum:        2,
		AudioCacheMs:       1000,
		RingBufferSize:     16,
		NoPublisherTimeout: time.Hour,
	}
	stream := NewStream("live/policy", cfg, config.LimitsConfig{}, NewEventBus())
	stream.WriteFrame(&avframe.AVFrame{MediaType: avframe.MediaTypeVideo, FrameType: avframe.FrameTypeKeyframe, DTS: 0})
	stream.WriteFrame(&avframe.AVFrame{MediaType: avframe.MediaTypeAudio, FrameType: avframe.FrameTypeInterframe, DTS: 1})
	if stream.GOPCacheLen() == 0 || len(stream.AudioCache()) == 0 {
		t.Fatal("test setup did not populate caches")
	}
	stream.RemovePublisher()

	next := cfg
	next.GOPCache = false
	next.AudioCacheMs = 0
	next.NoPublisherTimeout = 10 * time.Millisecond
	stream.UpdatePolicy(next, config.LimitsConfig{})
	if stream.GOPCacheLen() != 0 || len(stream.AudioCache()) != 0 {
		t.Fatalf("disabled caches retained data: gop=%d audio=%d", stream.GOPCacheLen(), len(stream.AudioCache()))
	}
	deadline := time.Now().Add(time.Second)
	for stream.State() != StreamStateDestroying && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if stream.State() != StreamStateDestroying {
		t.Fatal("updated no-publisher timeout did not replace the active timer")
	}
}
