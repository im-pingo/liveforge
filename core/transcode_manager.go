package core

import (
	"context"
	"sync"

	"github.com/im-pingo/liveforge/pkg/audiocodec"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/util"
)

// TranscodedTrack holds a ring buffer for a specific target codec.
type TranscodedTrack struct {
	targetCodec avframe.CodecType
	ringBuffer  *util.RingBuffer[*avframe.AVFrame]
	subCount    int
	cancel      context.CancelFunc
}

// TranscodeManager creates and manages on-demand audio transcoding goroutines.
// It is attached to a Stream and creates TranscodedTracks lazily when a subscriber
// requests a codec different from the publisher's.
type TranscodeManager struct {
	mu       sync.Mutex
	tracks   map[avframe.CodecType]*TranscodedTrack
	stream   *Stream
	registry *audiocodec.Registry
	bufSize  int
}

// NewTranscodeManager creates a TranscodeManager for the given stream.
func NewTranscodeManager(stream *Stream, registry *audiocodec.Registry, bufSize int) *TranscodeManager {
	return &TranscodeManager{
		tracks:   make(map[avframe.CodecType]*TranscodedTrack),
		stream:   stream,
		registry: registry,
		bufSize:  bufSize,
	}
}

// GetOrCreateReader returns a reader for the given target codec.
// If the publisher's codec matches, it returns the original ring buffer reader (zero overhead).
// Otherwise it creates or reuses a shared TranscodedTrack.
// The returned func must be called to release the subscription.
func (tm *TranscodeManager) GetOrCreateReader(targetCodec avframe.CodecType) (*util.RingReader[*avframe.AVFrame], func(), error) {
	// Stub — full implementation in Task 9
	return nil, func() {}, nil
}

// Reset cancels all active transcode goroutines and removes all tracks.
// Called when a new publisher replaces the old one.
func (tm *TranscodeManager) Reset() {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	for codec, track := range tm.tracks {
		if track.cancel != nil {
			track.cancel()
		}
		delete(tm.tracks, codec)
	}
}

// SetTranscodeManagerForTest sets the TranscodeManager on a Stream (for integration tests).
func SetTranscodeManagerForTest(s *Stream, tm *TranscodeManager) {
	s.transcodeManager = tm
}
