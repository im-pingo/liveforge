package core

import (
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/pkg/audiocodec"
	"github.com/im-pingo/liveforge/pkg/avframe"
)

// TestTranscodeManagerZeroOverhead verifies no TranscodedTrack is created
// when the subscriber requests the same codec as the publisher.
func TestTranscodeManagerZeroOverhead(t *testing.T) {
	s := newTranscodeTestStream(avframe.CodecAAC)
	tm := s.TranscodeManager()

	reader, release, err := tm.GetOrCreateReader(avframe.CodecAAC)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer release()

	if reader == nil {
		t.Fatal("expected non-nil reader")
	}
	if len(tm.tracks) != 0 {
		t.Fatalf("expected 0 tracks, got %d", len(tm.tracks))
	}
}

// TestTranscodeManagerCreateTrack verifies a TranscodedTrack is created when codecs differ.
func TestTranscodeManagerCreateTrack(t *testing.T) {
	s := newTranscodeTestStream(avframe.CodecG711U)
	tm := s.TranscodeManager()

	reader, release, err := tm.GetOrCreateReader(avframe.CodecG711A)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer release()

	if reader == nil {
		t.Fatal("expected non-nil reader")
	}
	if len(tm.tracks) != 1 {
		t.Fatalf("expected 1 track, got %d", len(tm.tracks))
	}
}

// TestTranscodeManagerSharing verifies two subscribers share one track.
func TestTranscodeManagerSharing(t *testing.T) {
	s := newTranscodeTestStream(avframe.CodecG711U)
	tm := s.TranscodeManager()

	_, release1, _ := tm.GetOrCreateReader(avframe.CodecG711A)
	_, release2, _ := tm.GetOrCreateReader(avframe.CodecG711A)

	if len(tm.tracks) != 1 {
		t.Fatalf("expected 1 shared track, got %d", len(tm.tracks))
	}
	if tm.tracks[avframe.CodecG711A].subCount != 2 {
		t.Fatalf("expected subCount=2, got %d", tm.tracks[avframe.CodecG711A].subCount)
	}

	release1()
	if tm.tracks[avframe.CodecG711A].subCount != 1 {
		t.Fatalf("expected subCount=1 after release1")
	}

	release2()
	time.Sleep(50 * time.Millisecond)
	if len(tm.tracks) != 0 {
		t.Fatalf("expected 0 tracks after all releases, got %d", len(tm.tracks))
	}
}

// TestTranscodeManagerReset verifies all tracks are cleaned up on Reset.
func TestTranscodeManagerReset(t *testing.T) {
	s := newTranscodeTestStream(avframe.CodecG711U)
	tm := s.TranscodeManager()

	_, _, _ = tm.GetOrCreateReader(avframe.CodecG711A)
	_, _, _ = tm.GetOrCreateReader(avframe.CodecG722)

	tm.Reset()
	time.Sleep(50 * time.Millisecond)

	if len(tm.tracks) != 0 {
		t.Fatalf("expected 0 tracks after Reset, got %d", len(tm.tracks))
	}
}

// TestTranscodeManagerNoPublisher verifies error when no publisher.
func TestTranscodeManagerNoPublisher(t *testing.T) {
	cfg := config.StreamConfig{RingBufferSize: 64}
	limits := config.LimitsConfig{}
	bus := NewEventBus()
	s := NewStream("test", cfg, limits, bus)

	tm := NewTranscodeManager(s, audiocodec.Global(), 64)
	_, _, err := tm.GetOrCreateReader(avframe.CodecOpus)
	if err == nil {
		t.Fatal("expected error when no publisher")
	}
}

// newTranscodeTestStream creates a test stream with a publisher of the given audio codec.
func newTranscodeTestStream(codec avframe.CodecType) *Stream {
	cfg := config.StreamConfig{RingBufferSize: 64}
	limits := config.LimitsConfig{}
	bus := NewEventBus()
	s := NewStream("test", cfg, limits, bus)
	s.transcodeManager = NewTranscodeManager(s, audiocodec.Global(), 64)

	pub := &testPublisher{
		id:   "test-pub",
		info: &avframe.MediaInfo{AudioCodec: codec},
	}
	_ = s.SetPublisher(pub)
	return s
}
