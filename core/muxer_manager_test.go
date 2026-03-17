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

func TestMuxerManagerStartCallback(t *testing.T) {
	bus := NewEventBus()
	cfg := newTestStreamConfig()
	stream := NewStream("live/test", cfg, bus)
	mm := NewMuxerManager(stream, 256)

	started := false
	mm.RegisterMuxerStart("flv", func(inst *MuxerInstance, s *Stream) {
		started = true
	})

	mm.GetOrCreateMuxer("flv")
	if !started {
		t.Error("start callback was not invoked")
	}

	// Second subscriber should NOT re-trigger callback
	started = false
	mm.GetOrCreateMuxer("flv")
	if started {
		t.Error("start callback should not fire for existing muxer")
	}
}

func TestMuxerInstanceDoneChannel(t *testing.T) {
	bus := NewEventBus()
	cfg := newTestStreamConfig()
	stream := NewStream("live/test", cfg, bus)
	mm := NewMuxerManager(stream, 256)

	var capturedInst *MuxerInstance
	mm.RegisterMuxerStart("flv", func(inst *MuxerInstance, s *Stream) {
		capturedInst = inst
	})

	mm.GetOrCreateMuxer("flv")

	select {
	case <-capturedInst.Done:
		t.Fatal("Done should not be closed yet")
	default:
	}

	mm.ReleaseMuxer("flv")

	select {
	case <-capturedInst.Done:
		// success
	default:
		t.Fatal("Done should be closed after last release")
	}
}

func TestMuxerInstanceInitData(t *testing.T) {
	bus := NewEventBus()
	cfg := newTestStreamConfig()
	stream := NewStream("live/test", cfg, bus)
	mm := NewMuxerManager(stream, 256)

	mm.RegisterMuxerStart("flv", func(inst *MuxerInstance, s *Stream) {
		inst.SetInitData([]byte("FLV-HEADER"))
	})

	_, inst := mm.GetOrCreateMuxer("flv")
	if string(inst.InitData()) != "FLV-HEADER" {
		t.Errorf("expected FLV-HEADER, got %s", inst.InitData())
	}

	// SetInitData should be idempotent
	inst.SetInitData([]byte("OTHER"))
	if string(inst.InitData()) != "FLV-HEADER" {
		t.Error("SetInitData should only work once")
	}
}
