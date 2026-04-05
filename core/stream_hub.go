package core

import (
	"fmt"
	"sync"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/pkg/audiocodec"
)

// StreamHub manages all active streams.
type StreamHub struct {
	mu               sync.RWMutex
	streams          map[string]*Stream
	config           config.StreamConfig
	limits           config.LimitsConfig
	eventBus         *EventBus
	audioCodecEnabled bool
}

// NewStreamHub creates a new StreamHub.
func NewStreamHub(cfg config.StreamConfig, limits config.LimitsConfig, bus *EventBus) *StreamHub {
	return &StreamHub{
		streams:  make(map[string]*Stream),
		config:   cfg,
		limits:   limits,
		eventBus: bus,
	}
}

// SetAudioCodecEnabled enables audio transcoding for new streams.
func (h *StreamHub) SetAudioCodecEnabled(enabled bool) {
	h.audioCodecEnabled = enabled
}

// GetOrCreate returns an existing stream or creates a new one.
// Returns an error if max_streams limit is reached and the stream does not already exist.
func (h *StreamHub) GetOrCreate(key string) (*Stream, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if s, ok := h.streams[key]; ok {
		if s.State() != StreamStateDestroying {
			return s, nil
		}
		// Stream is being destroyed; replace it with a fresh one.
		delete(h.streams, key)
	}

	if max := h.limits.MaxStreams; max > 0 && len(h.streams) >= max {
		return nil, fmt.Errorf("max streams limit reached (%d)", max)
	}

	s := NewStream(key, h.config, h.limits, h.eventBus)
	if h.audioCodecEnabled {
		s.transcodeManager = NewTranscodeManager(s, audiocodec.Global(), h.config.RingBufferSize)
	}
	h.streams[key] = s

	h.eventBus.Emit(EventStreamCreate, &EventContext{StreamKey: key}) //nolint:errcheck

	return s, nil
}

// Find returns a stream by key, or nil if not found.
func (h *StreamHub) Find(key string) (*Stream, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	s, ok := h.streams[key]
	return s, ok
}

// Remove deletes a stream from the hub and emits EventStreamDestroy.
func (h *StreamHub) Remove(key string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.streams[key]; ok {
		delete(h.streams, key)
		h.eventBus.Emit(EventStreamDestroy, &EventContext{StreamKey: key}) //nolint:errcheck
	}
}

// Count returns the number of active streams.
func (h *StreamHub) Count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.streams)
}

// Keys returns all active stream keys.
func (h *StreamHub) Keys() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	keys := make([]string, 0, len(h.streams))
	for k := range h.streams {
		keys = append(keys, k)
	}
	return keys
}
