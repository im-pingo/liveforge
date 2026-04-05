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
	tm.mu.Lock()
	trackCount := len(tm.tracks)
	tm.mu.Unlock()
	if trackCount != 0 {
		t.Fatalf("expected 0 tracks, got %d", trackCount)
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
	tm.mu.Lock()
	trackCount := len(tm.tracks)
	tm.mu.Unlock()
	if trackCount != 1 {
		t.Fatalf("expected 1 track, got %d", trackCount)
	}
}

// TestTranscodeManagerSharing verifies two subscribers share one track.
func TestTranscodeManagerSharing(t *testing.T) {
	s := newTranscodeTestStream(avframe.CodecG711U)
	tm := s.TranscodeManager()

	_, release1, _ := tm.GetOrCreateReader(avframe.CodecG711A)
	_, release2, _ := tm.GetOrCreateReader(avframe.CodecG711A)

	tm.mu.Lock()
	trackCount := len(tm.tracks)
	subCount := 0
	if track, ok := tm.tracks[avframe.CodecG711A]; ok {
		subCount = track.subCount
	}
	tm.mu.Unlock()
	if trackCount != 1 {
		t.Fatalf("expected 1 shared track, got %d", trackCount)
	}
	if subCount != 2 {
		t.Fatalf("expected subCount=2, got %d", subCount)
	}

	release1()
	tm.mu.Lock()
	subCount = 0
	if track, ok := tm.tracks[avframe.CodecG711A]; ok {
		subCount = track.subCount
	}
	tm.mu.Unlock()
	if subCount != 1 {
		t.Fatalf("expected subCount=1 after release1")
	}

	release2()
	time.Sleep(50 * time.Millisecond)
	tm.mu.Lock()
	trackCount = len(tm.tracks)
	tm.mu.Unlock()
	if trackCount != 0 {
		t.Fatalf("expected 0 tracks after all releases, got %d", trackCount)
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

	tm.mu.Lock()
	trackCount := len(tm.tracks)
	tm.mu.Unlock()
	if trackCount != 0 {
		t.Fatalf("expected 0 tracks after Reset, got %d", trackCount)
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
