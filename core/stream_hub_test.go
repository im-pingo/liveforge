package core

import (
	"testing"

	"github.com/im-pingo/liveforge/config"
)

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
