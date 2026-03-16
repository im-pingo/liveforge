package core

import (
	"testing"
)

func TestMuxerManagerGetOrCreate(t *testing.T) {
	bus := NewEventBus()
	cfg := newTestStreamConfig()
	stream := NewStream("live/test", cfg, bus)
	mm := NewMuxerManager(stream, 256)

	r1, inst1 := mm.GetOrCreateMuxer("flv")
	r2, inst2 := mm.GetOrCreateMuxer("flv")

	if inst1 != inst2 {
		t.Error("expected same muxer instance for same format")
	}
	if r1 == r2 {
		t.Error("expected different readers")
	}
	if mm.SubscriberCount("flv") != 2 {
		t.Errorf("expected 2 subscribers, got %d", mm.SubscriberCount("flv"))
	}
}

func TestMuxerManagerRelease(t *testing.T) {
	bus := NewEventBus()
	cfg := newTestStreamConfig()
	stream := NewStream("live/test", cfg, bus)
	mm := NewMuxerManager(stream, 256)

	mm.GetOrCreateMuxer("flv")
	mm.GetOrCreateMuxer("flv")

	mm.ReleaseMuxer("flv")
	if mm.SubscriberCount("flv") != 1 {
		t.Errorf("expected 1 subscriber, got %d", mm.SubscriberCount("flv"))
	}

	mm.ReleaseMuxer("flv")
	if mm.SubscriberCount("flv") != 0 {
		t.Errorf("expected 0 subscribers, got %d", mm.SubscriberCount("flv"))
	}
}
