package core

import (
	"sync"

	"github.com/im-pingo/liveforge/config"
)

// StreamHub manages all active streams.
type StreamHub struct {
	mu       sync.RWMutex
	streams  map[string]*Stream
	config   config.StreamConfig
	eventBus *EventBus
}

// NewStreamHub creates a new StreamHub.
func NewStreamHub(cfg config.StreamConfig, bus *EventBus) *StreamHub {
	return &StreamHub{
		streams:  make(map[string]*Stream),
		config:   cfg,
		eventBus: bus,
	}
}

// GetOrCreate returns an existing stream or creates a new one.
// If the existing stream is in the destroying state, it is replaced with a fresh stream.
func (h *StreamHub) GetOrCreate(key string) *Stream {
	h.mu.Lock()
	defer h.mu.Unlock()

	if s, ok := h.streams[key]; ok {
		if s.State() != StreamStateDestroying {
			return s
		}
		// Stream is being destroyed; replace it with a fresh one.
		delete(h.streams, key)
	}

	s := NewStream(key, h.config, h.eventBus)
	h.streams[key] = s
	return s
}

// Find returns a stream by key, or nil if not found.
func (h *StreamHub) Find(key string) (*Stream, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	s, ok := h.streams[key]
	return s, ok
}

// Remove deletes a stream from the hub.
func (h *StreamHub) Remove(key string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.streams, key)
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
