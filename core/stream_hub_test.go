package core

import (
	"strings"
	"testing"

	"github.com/im-pingo/liveforge/config"
)

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
