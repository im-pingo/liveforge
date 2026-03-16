package core

import (
	"sync"
)

// MuxerInstance holds a SharedBuffer and tracks subscriber count for a format.
type MuxerInstance struct {
	Buffer    *SharedBuffer
	subCount  int
}

// MuxerManager manages per-format muxer instances for a stream.
type MuxerManager struct {
	mu      sync.Mutex
	muxers  map[string]*MuxerInstance
	stream  *Stream
	bufSize int
}

// NewMuxerManager creates a new muxer manager for a stream.
func NewMuxerManager(stream *Stream, bufSize int) *MuxerManager {
	return &MuxerManager{
		muxers:  make(map[string]*MuxerInstance),
		stream:  stream,
		bufSize: bufSize,
	}
}

// GetOrCreateMuxer returns (or creates) a muxer instance for the given format,
// and returns a new SharedBuffer reader for the caller.
func (mm *MuxerManager) GetOrCreateMuxer(format string) (*SharedBufferReader, *MuxerInstance) {
	mm.mu.Lock()
	defer mm.mu.Unlock()

	inst, ok := mm.muxers[format]
	if !ok {
		inst = &MuxerInstance{
			Buffer: NewSharedBuffer(mm.bufSize),
		}
		mm.muxers[format] = inst
	}
	inst.subCount++

	return inst.Buffer.NewReader(), inst
}

// ReleaseMuxer decrements the subscriber count for a format.
// If count reaches zero, the muxer instance is removed.
func (mm *MuxerManager) ReleaseMuxer(format string) {
	mm.mu.Lock()
	defer mm.mu.Unlock()

	inst, ok := mm.muxers[format]
	if !ok {
		return
	}
	inst.subCount--
	if inst.subCount <= 0 {
		delete(mm.muxers, format)
	}
}

// SubscriberCount returns the number of subscribers for a given format.
func (mm *MuxerManager) SubscriberCount(format string) int {
	mm.mu.Lock()
	defer mm.mu.Unlock()

	inst, ok := mm.muxers[format]
	if !ok {
		return 0
	}
	return inst.subCount
}
