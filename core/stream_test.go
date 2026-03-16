package core

import (
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/pkg/avframe"
)

func newTestStreamConfig() config.StreamConfig {
	return config.StreamConfig{
		GOPCache:           true,
		GOPCacheNum:        1,
		AudioCacheMs:       1000,
		RingBufferSize:     256,
		IdleTimeout:        5 * time.Second,
		NoPublisherTimeout: 3 * time.Second,
	}
}

type testPublisher struct {
	id   string
	info *avframe.MediaInfo
}

func (p *testPublisher) ID() string                   { return p.id }
func (p *testPublisher) MediaInfo() *avframe.MediaInfo { return p.info }
func (p *testPublisher) Close() error                  { return nil }

func TestStreamStateTransitions(t *testing.T) {
	bus := NewEventBus()
	s := NewStream("live/test", newTestStreamConfig(), bus)

	if s.State() != StreamStateIdle {
		t.Fatalf("expected idle, got %v", s.State())
	}

	pub := &testPublisher{
		id:   "pub1",
		info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264, AudioCodec: avframe.CodecAAC},
	}
	err := s.SetPublisher(pub)
	if err != nil {
		t.Fatalf("SetPublisher error: %v", err)
	}
	if s.State() != StreamStatePublishing {
		t.Fatalf("expected publishing, got %v", s.State())
	}

	s.RemovePublisher()
	if s.State() != StreamStateNoPublisher {
		t.Fatalf("expected no_publisher, got %v", s.State())
	}
}

func TestStreamRejectDuplicatePublisher(t *testing.T) {
	bus := NewEventBus()
	s := NewStream("live/test", newTestStreamConfig(), bus)

	pub1 := &testPublisher{id: "pub1", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}
	pub2 := &testPublisher{id: "pub2", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}

	_ = s.SetPublisher(pub1)
	err := s.SetPublisher(pub2)
	if err == nil {
		t.Error("expected error for duplicate publisher")
	}
}

func TestStreamWriteAndReadFrames(t *testing.T) {
	bus := NewEventBus()
	s := NewStream("live/test", newTestStreamConfig(), bus)

	pub := &testPublisher{id: "pub1", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264, AudioCodec: avframe.CodecAAC}}
	_ = s.SetPublisher(pub)

	// Write frames
	keyframe := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 0, 0, []byte{0x65, 0x01})
	s.WriteFrame(keyframe)

	audio := avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe, 0, 0, []byte{0xFF, 0x01})
	s.WriteFrame(audio)

	inter := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe, 40, 40, []byte{0x41, 0x01})
	s.WriteFrame(inter)

	// Verify GOP cache
	gop := s.GOPCache()
	if len(gop) < 1 {
		t.Fatal("expected at least 1 frame in GOP cache")
	}
	if gop[0].FrameType != avframe.FrameTypeKeyframe {
		t.Error("first frame in GOP should be keyframe")
	}
}

func TestStreamNoPublisherTimeout(t *testing.T) {
	bus := NewEventBus()
	cfg := newTestStreamConfig()
	cfg.NoPublisherTimeout = 100 * time.Millisecond // short timeout for test
	s := NewStream("live/timeout", cfg, bus)

	pub := &testPublisher{id: "pub1", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}
	_ = s.SetPublisher(pub)
	s.RemovePublisher()

	if s.State() != StreamStateNoPublisher {
		t.Fatalf("expected no_publisher, got %v", s.State())
	}

	// Wait for timeout
	time.Sleep(200 * time.Millisecond)
	if s.State() != StreamStateDestroying {
		t.Errorf("expected destroying after timeout, got %v", s.State())
	}
}

func TestStreamRepublishBeforeTimeout(t *testing.T) {
	bus := NewEventBus()
	cfg := newTestStreamConfig()
	cfg.NoPublisherTimeout = 500 * time.Millisecond
	s := NewStream("live/republish", cfg, bus)

	pub1 := &testPublisher{id: "pub1", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}
	_ = s.SetPublisher(pub1)
	s.RemovePublisher()

	// Republish before timeout with same codec
	pub2 := &testPublisher{id: "pub2", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}
	err := s.SetPublisher(pub2)
	if err != nil {
		t.Fatalf("republish should succeed: %v", err)
	}
	if s.State() != StreamStatePublishing {
		t.Errorf("expected publishing after republish, got %v", s.State())
	}
}
