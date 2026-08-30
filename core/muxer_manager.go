package core

import (
	"sync"
)

// MuxerStartFunc is invoked when a muxer instance is first created (first subscriber).
type MuxerStartFunc func(inst *MuxerInstance, stream *Stream)

// MuxerInstance holds a SharedBuffer and tracks subscriber count for a format.
type MuxerInstance struct {
	Buffer     *SharedBuffer
	Generation uint64
	subCount   int
	Done       chan struct{} // closed when last subscriber leaves or its generation is retired
	doneOnce   sync.Once
	initMu     sync.Mutex
	initDone   bool
	initData   []byte
}

func (inst *MuxerInstance) close() {
	inst.doneOnce.Do(func() {
		close(inst.Done)
	})
}

// SetInitData stores format-specific init data (only the first call takes effect).
func (inst *MuxerInstance) SetInitData(data []byte) {
	inst.initMu.Lock()
	defer inst.initMu.Unlock()
	if !inst.initDone {
		inst.initData = data
		inst.initDone = true
	}
}

// InitData returns the stored init data. May return nil if not yet set.
func (inst *MuxerInstance) InitData() []byte {
	inst.initMu.Lock()
	defer inst.initMu.Unlock()
	return inst.initData
}

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
// It returns (nil, nil) when the stream has no active publisher generation.
func (mm *MuxerManager) GetOrCreateMuxer(format string) (*SharedBufferReader, *MuxerInstance) {
	mm.mu.Lock()
	defer mm.mu.Unlock()

	generation, active := mm.stream.activePublisherGeneration()
	return mm.getOrCreateMuxerLocked(format, generation, active)
}

// GetOrCreateMuxerForGeneration returns a muxer only while the requested
// publisher generation is still active.
func (mm *MuxerManager) GetOrCreateMuxerForGeneration(format string, generation uint64) (*SharedBufferReader, *MuxerInstance) {
	mm.mu.Lock()
	defer mm.mu.Unlock()

	activeGeneration, active := mm.stream.activePublisherGeneration()
	if !active || activeGeneration != generation {
		return nil, nil
	}
	return mm.getOrCreateMuxerLocked(format, generation, true)
}

func (mm *MuxerManager) getOrCreateMuxerLocked(format string, generation uint64, active bool) (*SharedBufferReader, *MuxerInstance) {
	inst, ok := mm.muxers[format]
	if !active {
		if ok {
			inst.close()
			delete(mm.muxers, format)
		}
		return nil, nil
	}
	isNew := !ok || inst.Generation != generation
	if isNew {
		if ok {
			inst.close()
		}
		inst = &MuxerInstance{
			Buffer:     NewSharedBuffer(mm.bufSize),
			Generation: generation,
			Done:       make(chan struct{}),
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
func (mm *MuxerManager) ReleaseMuxer(format string, inst *MuxerInstance) {
	mm.mu.Lock()
	defer mm.mu.Unlock()

	if inst == nil || inst.subCount <= 0 {
		return
	}
	inst.subCount--
	if inst.subCount <= 0 {
		inst.close()
		if current, ok := mm.muxers[format]; ok && current == inst {
			delete(mm.muxers, format)
		}
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
