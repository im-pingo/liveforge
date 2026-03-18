package core

import (
	"sync"
)

// MuxerStartFunc is invoked when a muxer instance is first created (first subscriber).
type MuxerStartFunc func(inst *MuxerInstance, stream *Stream)

// MuxerInstance holds a SharedBuffer and tracks subscriber count for a format.
type MuxerInstance struct {
	Buffer   *SharedBuffer
	subCount int
	Done     chan struct{} // closed when last subscriber leaves
	initOnce sync.Once
	initData []byte
}

// SetInitData stores format-specific init data (thread-safe via sync.Once).
func (inst *MuxerInstance) SetInitData(data []byte) {
	inst.initOnce.Do(func() { inst.initData = data })
}

// InitData returns the stored init data. May return nil if not yet set.
func (inst *MuxerInstance) InitData() []byte { return inst.initData }

// MuxerManager manages per-format muxer instances for a stream.
type MuxerManager struct {
	mu      sync.Mutex
	muxers  map[string]*MuxerInstance
	stream  *Stream
	bufSize int
	onStart map[string]MuxerStartFunc
}

// NewMuxerManager creates a new muxer manager for a stream.
func NewMuxerManager(stream *Stream, bufSize int) *MuxerManager {
	return &MuxerManager{
		muxers:  make(map[string]*MuxerInstance),
		stream:  stream,
		bufSize: bufSize,
		onStart: make(map[string]MuxerStartFunc),
	}
}

// RegisterMuxerStart registers a callback invoked when the first subscriber
// for a format arrives and a new muxer instance is created.
func (mm *MuxerManager) RegisterMuxerStart(format string, fn MuxerStartFunc) {
	mm.mu.Lock()
	defer mm.mu.Unlock()
	mm.onStart[format] = fn
}

// GetOrCreateMuxer returns (or creates) a muxer instance for the given format,
// and returns a new SharedBuffer reader for the caller.
// If a new instance is created, the registered start callback is invoked.
func (mm *MuxerManager) GetOrCreateMuxer(format string) (*SharedBufferReader, *MuxerInstance) {
	mm.mu.Lock()
	defer mm.mu.Unlock()

	inst, ok := mm.muxers[format]
	isNew := !ok
	if isNew {
		inst = &MuxerInstance{
			Buffer: NewSharedBuffer(mm.bufSize),
			Done:   make(chan struct{}),
		}
		mm.muxers[format] = inst
	}
	inst.subCount++

	if isNew {
		if fn, exists := mm.onStart[format]; exists {
			fn(inst, mm.stream)
		}
	}

	return inst.Buffer.NewReader(), inst
}

// ReleaseMuxer decrements the subscriber count for a format.
// If count reaches zero, the Done channel is closed and the muxer instance is removed.
func (mm *MuxerManager) ReleaseMuxer(format string) {
	mm.mu.Lock()
	defer mm.mu.Unlock()

	inst, ok := mm.muxers[format]
	if !ok {
		return
	}
	inst.subCount--
	if inst.subCount <= 0 {
		close(inst.Done)
		delete(mm.muxers, format)
	}
}

// Formats returns a map of active format names to their subscriber counts.
func (mm *MuxerManager) Formats() map[string]int {
	mm.mu.Lock()
	defer mm.mu.Unlock()

	result := make(map[string]int, len(mm.muxers))
	for format, inst := range mm.muxers {
		result[format] = inst.subCount
	}
	return result
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
