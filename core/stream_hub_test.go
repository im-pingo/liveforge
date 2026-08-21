package core

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/pkg/avframe"
)

func TestStreamHubConcurrentConfigAndCreation(t *testing.T) {
	hub := NewStreamHub(newTestStreamConfig(), config.LimitsConfig{MaxStreams: 1000}, NewEventBus())
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				cfg := newTestStreamConfig()
				cfg.RingBufferSize = 16 + j%4
				hub.UpdateNewSessionConfig(cfg, config.LimitsConfig{MaxStreams: 1000}, j%2 == 0)
				_, _ = hub.GetOrCreate(fmt.Sprintf("live/concurrent-%d-%d", worker, j))
			}
		}(i)
	}
	wg.Wait()
}

func TestStreamHubRejectsUnsafeStreamKeys(t *testing.T) {
	hub := NewStreamHub(newTestStreamConfig(), config.LimitsConfig{}, NewEventBus())
	for _, key := range []string{"", "/absolute", "../escape", "live/../escape", "live//camera", `live\..\escape`, "live/camera?token=x", "live/\x00camera"} {
		t.Run(strings.ReplaceAll(key, "/", "_"), func(t *testing.T) {
			if _, err := hub.GetOrCreate(key); err == nil {
				t.Fatalf("GetOrCreate(%q) succeeded", key)
			}
		})
	}
}

func TestStreamHubCreateAndFind(t *testing.T) {
	bus := NewEventBus()
	cfg := newTestStreamConfig()
	hub := NewStreamHub(cfg, config.LimitsConfig{}, bus)

	s1, err := hub.GetOrCreate("live/room1")
	if err != nil {
		t.Fatal(err)
	}
	s2, err := hub.GetOrCreate("live/room1")
	if err != nil {
		t.Fatal(err)
	}
	if s1 != s2 {
		t.Error("expected same stream instance for same key")
	}

	s3, err := hub.GetOrCreate("live/room2")
	if err != nil {
		t.Fatal(err)
	}
	if s1 == s3 {
		t.Error("expected different stream for different key")
	}

	if hub.Count() != 2 {
		t.Errorf("expected 2 streams, got %d", hub.Count())
	}
}

func TestStreamHubRemove(t *testing.T) {
	bus := NewEventBus()
	cfg := newTestStreamConfig()
	hub := NewStreamHub(cfg, config.LimitsConfig{}, bus)

	hub.GetOrCreate("live/room1")
	hub.Remove("live/room1")

	if hub.Count() != 0 {
		t.Errorf("expected 0 streams after remove, got %d", hub.Count())
	}
}

func TestStreamHubList(t *testing.T) {
	bus := NewEventBus()
	cfg := newTestStreamConfig()
	hub := NewStreamHub(cfg, config.LimitsConfig{}, bus)

	hub.GetOrCreate("live/a")
	hub.GetOrCreate("live/b")

	keys := hub.Keys()
	if len(keys) != 2 {
		t.Errorf("expected 2 keys, got %d", len(keys))
	}
}

func TestStreamHubFind(t *testing.T) {
	bus := NewEventBus()
	cfg := newTestStreamConfig()
	hub := NewStreamHub(cfg, config.LimitsConfig{}, bus)

	// Find non-existent
	_, ok := hub.Find("live/nonexistent")
	if ok {
		t.Error("expected Find to return false for non-existent stream")
	}

	// Create and find
	s, _ := hub.GetOrCreate("live/findme")
	found, ok := hub.Find("live/findme")
	if !ok {
		t.Error("expected Find to return true for existing stream")
	}
	if found != s {
		t.Error("expected Find to return same stream instance")
	}
}

func TestStreamHubGetOrCreateReplacesDestroying(t *testing.T) {
	bus := NewEventBus()
	cfg := newTestStreamConfig()
	hub := NewStreamHub(cfg, config.LimitsConfig{}, bus)

	s1, _ := hub.GetOrCreate("live/replace")
	s1.Close() // set to destroying

	s2, err := hub.GetOrCreate("live/replace")
	if err != nil {
		t.Fatal(err)
	}
	if s1 == s2 {
		t.Error("expected new stream after destroying")
	}
}

func TestStreamHubMaxStreams(t *testing.T) {
	bus := NewEventBus()
	cfg := newTestStreamConfig()
	limits := config.LimitsConfig{MaxStreams: 2}
	hub := NewStreamHub(cfg, limits, bus)

	if _, err := hub.GetOrCreate("live/a"); err != nil {
		t.Fatal(err)
	}
	if _, err := hub.GetOrCreate("live/b"); err != nil {
		t.Fatal(err)
	}
	if _, err := hub.GetOrCreate("live/c"); err == nil {
		t.Error("expected error when exceeding max_streams limit")
	}
}

func TestStreamHubRuntimeConfigAppliesToNewStreamsOnly(t *testing.T) {
	bus := NewEventBus()
	initial := config.StreamConfig{RingBufferSize: 16, AudioCacheMs: 100}
	hub := NewStreamHub(initial, config.LimitsConfig{MaxStreams: 2}, bus)
	old, err := hub.GetOrCreate("live/old")
	if err != nil {
		t.Fatal(err)
	}

	next := config.StreamConfig{RingBufferSize: 64, AudioCacheMs: 900}
	hub.UpdateNewSessionConfig(next, config.LimitsConfig{MaxStreams: 3}, true)

	if got := old.Config(); got.RingBufferSize != 16 || got.AudioCacheMs != 100 {
		t.Fatalf("existing stream config changed: %#v", got)
	}
	newStream, err := hub.GetOrCreate("live/new")
	if err != nil {
		t.Fatal(err)
	}
	if got := newStream.Config(); got.RingBufferSize != 64 || got.AudioCacheMs != 900 {
		t.Fatalf("new stream config = %#v, want updated config", got)
	}
	if _, err := hub.GetOrCreate("live/third"); err != nil {
		t.Fatalf("updated max streams was not applied: %v", err)
	}
}

func TestStreamHubDestroyIsConditionalAndExactlyOnce(t *testing.T) {
	bus := NewEventBus()
	destroyed := make(chan struct{}, 2)
	bus.Register(HookRegistration{Event: EventStreamDestroy, Mode: HookSync, Handler: func(*EventContext) error {
		destroyed <- struct{}{}
		return nil
	}})
	hub := NewStreamHub(newTestStreamConfig(), config.LimitsConfig{}, bus)
	stream, err := hub.GetOrCreate("live/destroy")
	if err != nil {
		t.Fatal(err)
	}
	other := NewStream("live/other", newTestStreamConfig(), config.LimitsConfig{}, bus)
	if hub.Destroy("live/destroy", other) {
		t.Fatal("destroy accepted a non-incumbent stream")
	}
	if !hub.Destroy("live/destroy", stream) {
		t.Fatal("destroy rejected the incumbent stream")
	}
	if hub.Destroy("live/destroy", stream) {
		t.Fatal("destroy emitted a second removal")
	}
	if hub.Count() != 0 {
		t.Fatalf("hub count = %d, want 0", hub.Count())
	}
	select {
	case <-destroyed:
	case <-time.After(time.Second):
		t.Fatal("stream destroy event was not emitted")
	}
	select {
	case <-destroyed:
		t.Fatal("stream destroy event emitted more than once")
	default:
	}
}

func TestStreamHubReclaimsTimedOutStream(t *testing.T) {
	bus := NewEventBus()
	cfg := newTestStreamConfig()
	cfg.NoPublisherTimeout = 20 * time.Millisecond
	hub := NewStreamHub(cfg, config.LimitsConfig{MaxStreams: 1}, bus)
	stream, err := hub.GetOrCreate("live/timeout-reclaim")
	if err != nil {
		t.Fatal(err)
	}
	pub := &testPublisher{id: "pub", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}
	if _, err := stream.SetPublisherWithGeneration(pub); err != nil {
		t.Fatal(err)
	}
	stream.RemovePublisher()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if hub.Count() == 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if hub.Count() != 0 {
		t.Fatal("timed out stream remained in hub")
	}
	if _, err := hub.GetOrCreate("live/reclaimed"); err != nil {
		t.Fatalf("max-stream slot was not reclaimed: %v", err)
	}
}
