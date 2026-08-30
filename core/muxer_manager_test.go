package core

import (
	"testing"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/pkg/avframe"
)

func newPublishingMuxerManager(t *testing.T, key string) (*Stream, *MuxerManager) {
	t.Helper()
	stream := NewStream(key, newTestStreamConfig(), config.LimitsConfig{}, NewEventBus())
	if err := stream.SetPublisher(&testPublisher{
		id:   "publisher",
		info: &avframe.MediaInfo{AudioCodec: avframe.CodecMP3},
	}); err != nil {
		t.Fatal(err)
	}
	return stream, NewMuxerManager(stream, 256)
}

func TestMuxerManagerGetOrCreate(t *testing.T) {
	_, mm := newPublishingMuxerManager(t, "live/test")

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
	_, mm := newPublishingMuxerManager(t, "live/test")

	_, inst := mm.GetOrCreateMuxer("flv")
	mm.GetOrCreateMuxer("flv")

	mm.ReleaseMuxer("flv", inst)
	if mm.SubscriberCount("flv") != 1 {
		t.Errorf("expected 1 subscriber, got %d", mm.SubscriberCount("flv"))
	}

	mm.ReleaseMuxer("flv", inst)
	if mm.SubscriberCount("flv") != 0 {
		t.Errorf("expected 0 subscribers, got %d", mm.SubscriberCount("flv"))
	}
}

func TestMuxerManagerStartCallback(t *testing.T) {
	_, mm := newPublishingMuxerManager(t, "live/test")

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
	_, mm := newPublishingMuxerManager(t, "live/test")

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

	mm.ReleaseMuxer("flv", capturedInst)

	select {
	case <-capturedInst.Done:
		// success
	default:
		t.Fatal("Done should be closed after last release")
	}
}

func TestMuxerManagerFormats(t *testing.T) {
	_, mm := newPublishingMuxerManager(t, "live/test")

	// Empty
	formats := mm.Formats()
	if len(formats) != 0 {
		t.Errorf("expected empty formats, got %d", len(formats))
	}

	// Add subscribers
	mm.GetOrCreateMuxer("flv")
	mm.GetOrCreateMuxer("flv")
	mm.GetOrCreateMuxer("ts")

	formats = mm.Formats()
	if formats["flv"] != 2 {
		t.Errorf("expected 2 flv subscribers, got %d", formats["flv"])
	}
	if formats["ts"] != 1 {
		t.Errorf("expected 1 ts subscriber, got %d", formats["ts"])
	}
}

func TestMuxerManagerReleaseNonExistent(t *testing.T) {
	bus := NewEventBus()
	cfg := newTestStreamConfig()
	stream := NewStream("live/test", cfg, config.LimitsConfig{}, bus)
	mm := NewMuxerManager(stream, 256)

	// Should not panic
	mm.ReleaseMuxer("nonexistent", nil)
}

func TestMuxerManagerRejectsBeforePublisher(t *testing.T) {
	stream := NewStream("live/muxer-before-publisher", newTestStreamConfig(), config.LimitsConfig{}, NewEventBus())
	mm := NewMuxerManager(stream, 256)
	starts := 0
	mm.RegisterMuxerStart("flv", func(inst *MuxerInstance, s *Stream) {
		starts++
	})

	reader, inst := mm.GetOrCreateMuxer("flv")
	if reader != nil || inst != nil {
		t.Fatalf("pre-publish muxer = (%p, %p), want (nil, nil)", reader, inst)
	}
	if starts != 0 {
		t.Fatalf("pre-publish start callbacks = %d, want 0", starts)
	}
	if got := mm.SubscriberCount("flv"); got != 0 {
		t.Fatalf("pre-publish subscribers = %d, want 0", got)
	}
}

func TestMuxerManagerRetiresOnPublisherRemovalBeforeRepublish(t *testing.T) {
	stream := NewStream("live/muxer-remove", newTestStreamConfig(), config.LimitsConfig{}, NewEventBus())
	pubA := &testPublisher{id: "publisher-a", info: &avframe.MediaInfo{AudioCodec: avframe.CodecMP3}}
	if err := stream.SetPublisher(pubA); err != nil {
		t.Fatal(err)
	}
	mm := NewMuxerManager(stream, 256)
	starts := 0
	mm.RegisterMuxerStart("flv", func(inst *MuxerInstance, s *Stream) {
		starts++
	})
	_, instA := mm.GetOrCreateMuxer("flv")

	if !stream.RemovePublisherIf(pubA) {
		t.Fatal("publisher A was not removed")
	}
	reader, inst := mm.GetOrCreateMuxer("flv")
	if reader != nil || inst != nil {
		t.Errorf("removed-publisher muxer = (%p, %p), want (nil, nil)", reader, inst)
	}
	select {
	case <-instA.Done:
	default:
		t.Error("ended generation muxer was not retired")
	}
	if got := mm.SubscriberCount("flv"); got != 0 {
		t.Errorf("removed-publisher subscribers = %d, want 0", got)
	}
	if starts != 1 {
		t.Errorf("removed-publisher start callbacks = %d, want 1", starts)
	}

	pubB := &testPublisher{id: "publisher-b", info: &avframe.MediaInfo{AudioCodec: avframe.CodecMP3}}
	if err := stream.SetPublisher(pubB); err != nil {
		t.Fatal(err)
	}
	_, instB := mm.GetOrCreateMuxer("flv")
	if instB == nil || instB == instA {
		t.Fatal("replacement publisher did not receive a fresh muxer")
	}
	if instB.Generation != 2 {
		t.Fatalf("replacement muxer generation = %d, want 2", instB.Generation)
	}
	if starts != 2 {
		t.Fatalf("replacement start callbacks = %d, want 2", starts)
	}
}

func TestMuxerManagerGenerationReplacementAndInstanceRelease(t *testing.T) {
	stream := NewStream("live/muxer-generation", newTestStreamConfig(), config.LimitsConfig{}, NewEventBus())
	pubA := &testPublisher{id: "publisher-a", info: &avframe.MediaInfo{AudioCodec: avframe.CodecMP3}}
	if err := stream.SetPublisher(pubA); err != nil {
		t.Fatal(err)
	}

	mm := NewMuxerManager(stream, 256)
	starts := 0
	mm.RegisterMuxerStart("flv", func(inst *MuxerInstance, s *Stream) {
		starts++
	})
	_, instA := mm.GetOrCreateMuxer("flv")
	mm.GetOrCreateMuxer("flv")
	if instA.Generation != 1 {
		t.Fatalf("publisher A muxer generation = %d, want 1", instA.Generation)
	}

	if !stream.RemovePublisherIf(pubA) {
		t.Fatal("publisher A was not removed")
	}
	pubB := &testPublisher{id: "publisher-b", info: &avframe.MediaInfo{AudioCodec: avframe.CodecMP3}}
	if err := stream.SetPublisher(pubB); err != nil {
		t.Fatal(err)
	}
	_, instB := mm.GetOrCreateMuxer("flv")
	if instB == instA {
		t.Fatal("replacement publisher reused the retired muxer instance")
	}
	if instB.Generation != 2 {
		t.Fatalf("publisher B muxer generation = %d, want 2", instB.Generation)
	}
	if starts != 2 {
		t.Fatalf("muxer start callbacks = %d, want 2", starts)
	}
	select {
	case <-instA.Done:
	default:
		t.Fatal("retired muxer instance was not closed")
	}

	mm.ReleaseMuxer("flv", instA)
	mm.ReleaseMuxer("flv", instA)
	if got := mm.SubscriberCount("flv"); got != 1 {
		t.Fatalf("retired instance release changed replacement subscribers to %d, want 1", got)
	}
	select {
	case <-instB.Done:
		t.Fatal("retired instance release closed the replacement muxer")
	default:
	}

	mm.ReleaseMuxer("flv", instB)
	if got := mm.SubscriberCount("flv"); got != 0 {
		t.Fatalf("replacement subscribers = %d after release, want 0", got)
	}
}

func TestMuxerManagerRejectsStaleRequestedGeneration(t *testing.T) {
	stream := NewStream("live/muxer-stale-request", newTestStreamConfig(), config.LimitsConfig{}, NewEventBus())
	pubA := &testPublisher{id: "publisher-a", info: &avframe.MediaInfo{AudioCodec: avframe.CodecMP3}}
	if err := stream.SetPublisher(pubA); err != nil {
		t.Fatal(err)
	}
	generationA := stream.StartupSnapshot().Generation
	if !stream.RemovePublisherIf(pubA) {
		t.Fatal("publisher A was not removed")
	}
	pubB := &testPublisher{id: "publisher-b", info: &avframe.MediaInfo{AudioCodec: avframe.CodecMP3}}
	if err := stream.SetPublisher(pubB); err != nil {
		t.Fatal(err)
	}
	mm := NewMuxerManager(stream, 256)
	starts := 0
	mm.RegisterMuxerStart("flv", func(*MuxerInstance, *Stream) { starts++ })

	reader, inst := mm.GetOrCreateMuxerForGeneration("flv", generationA)
	if reader != nil || inst != nil {
		t.Fatalf("stale generation muxer = (%p, %p), want nil", reader, inst)
	}
	if starts != 0 || mm.SubscriberCount("flv") != 0 {
		t.Fatalf("stale request started muxer: starts=%d subscribers=%d", starts, mm.SubscriberCount("flv"))
	}
}

func TestMuxerManagerSubscriberCountNonExistent(t *testing.T) {
	bus := NewEventBus()
	cfg := newTestStreamConfig()
	stream := NewStream("live/test", cfg, config.LimitsConfig{}, bus)
	mm := NewMuxerManager(stream, 256)

	if mm.SubscriberCount("nonexistent") != 0 {
		t.Error("expected 0 for non-existent format")
	}
}

func TestMuxerInstanceInitData(t *testing.T) {
	_, mm := newPublishingMuxerManager(t, "live/test")

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
