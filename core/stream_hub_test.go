package core

import (
	"testing"
	"time"

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

func TestStreamHubStableStreamsReturnsBoundedCreationOrder(t *testing.T) {
	hub := NewStreamHub(newTestStreamConfig(), config.LimitsConfig{}, NewEventBus())
	for _, key := range []string{"live/z", "live/a", "live/m"} {
		if _, err := hub.GetOrCreate(key); err != nil {
			t.Fatal(err)
		}
	}

	assertStreamKeys := func(want []string) {
		t.Helper()
		streams := hub.StableStreams(len(want))
		if len(streams) != len(want) {
			t.Fatalf("stable stream count = %d, want %d", len(streams), len(want))
		}
		for index, stream := range streams {
			if got := stream.Key(); got != want[index] {
				t.Fatalf("stable stream %d = %q, want %q", index, got, want[index])
			}
		}
	}

	assertStreamKeys([]string{"live/z", "live/a"})
	hub.Remove("live/z")
	assertStreamKeys([]string{"live/a", "live/m"})
	if got := hub.StableStreams(0); len(got) != 0 {
		t.Fatalf("zero-limit stable streams = %d, want 0", len(got))
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

func TestStreamHubRemovesStreamAfterPublisherTimeout(t *testing.T) {
	cfg := config.StreamConfig{RingBufferSize: 8, NoPublisherTimeout: 20 * time.Millisecond}
	hub := NewStreamHub(cfg, config.LimitsConfig{}, NewEventBus())
	stream, err := hub.GetOrCreate("live/timeout-removal")
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.SetPublisher(&testPublisher{id: "timeout-publisher"}); err != nil {
		t.Fatal(err)
	}
	stream.RemovePublisher()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, ok := hub.Find("live/timeout-removal"); !ok {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("publisher-timeout stream remained in hub")
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
