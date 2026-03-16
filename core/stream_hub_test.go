package core

import (
	"testing"
)

func TestStreamHubCreateAndFind(t *testing.T) {
	bus := NewEventBus()
	cfg := newTestStreamConfig()
	hub := NewStreamHub(cfg, bus)

	s1 := hub.GetOrCreate("live/room1")
	s2 := hub.GetOrCreate("live/room1")
	if s1 != s2 {
		t.Error("expected same stream instance for same key")
	}

	s3 := hub.GetOrCreate("live/room2")
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
	hub := NewStreamHub(cfg, bus)

	hub.GetOrCreate("live/room1")
	hub.Remove("live/room1")

	if hub.Count() != 0 {
		t.Errorf("expected 0 streams after remove, got %d", hub.Count())
	}
}

func TestStreamHubList(t *testing.T) {
	bus := NewEventBus()
	cfg := newTestStreamConfig()
	hub := NewStreamHub(cfg, bus)

	hub.GetOrCreate("live/a")
	hub.GetOrCreate("live/b")

	keys := hub.Keys()
	if len(keys) != 2 {
		t.Errorf("expected 2 keys, got %d", len(keys))
	}
}
