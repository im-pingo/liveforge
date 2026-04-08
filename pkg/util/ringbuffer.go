package util

import (
	"sync"
	"sync/atomic"
)

// RingBuffer is a generic single-producer, multi-consumer ring buffer.
// The writer advances atomically; each reader maintains its own cursor.
type RingBuffer[T any] struct {
	buf         []T
	size        int64
	writeCursor atomic.Int64 // next write position (monotonically increasing)
	signal      chan struct{}
	closed      atomic.Bool
	mu          sync.Mutex // protects cond for Read() blocking
	cond        *sync.Cond // wakes blocked Read() callers on Write/Close
}

// NewRingBuffer creates a new ring buffer with the given capacity.
func NewRingBuffer[T any](size int) *RingBuffer[T] {
	rb := &RingBuffer[T]{
		buf:    make([]T, size),
		size:   int64(size),
		signal: make(chan struct{}, 1),
	}
	rb.cond = sync.NewCond(&rb.mu)
	return rb
}

// Write adds a value to the ring buffer. If the buffer is full, the oldest
// value is silently overwritten. No-op if the buffer is closed.
func (rb *RingBuffer[T]) Write(val T) {
	if rb.closed.Load() {
		return
	}
	// Single-producer: store value first, then advance cursor so readers
	// never see an uninitialized slot.
	pos := rb.writeCursor.Load()
	rb.buf[pos%rb.size] = val
	rb.writeCursor.Store(pos + 1)

	// Wake all Read() callers blocked on cond.Wait()
	rb.cond.Broadcast()

	// Non-blocking notify for select-based consumers using Signal()
	select {
	case rb.signal <- struct{}{}:
	default:
	}
}

// Close signals all blocked readers to return (zero, false).
// After Close, Write is a no-op.
func (rb *RingBuffer[T]) Close() {
	rb.closed.Store(true)
	// Wake all Read() callers blocked on cond.Wait()
	rb.cond.Broadcast()
	// Wake select-based consumers using Signal()
	select {
	case rb.signal <- struct{}{}:
	default:
	}
}

// IsClosed returns whether the ring buffer has been closed.
func (rb *RingBuffer[T]) IsClosed() bool {
	return rb.closed.Load()
}

// Signal returns the notification channel that is signaled on each Write.
// Useful for select-based consumers that need to multiplex with other channels.
func (rb *RingBuffer[T]) Signal() <-chan struct{} {
	return rb.signal
}

// WriteCursor returns the current write position (number of items written).
func (rb *RingBuffer[T]) WriteCursor() int64 {
	return rb.writeCursor.Load()
}

// NewReader creates a new reader starting at the oldest available position.
func (rb *RingBuffer[T]) NewReader() *RingReader[T] {
	wc := rb.writeCursor.Load()
	start := wc - rb.size
	if start < 0 {
		start = 0
	}
	return &RingReader[T]{
		rb:         rb,
		readCursor: start,
	}
}

// NewReaderAt creates a new reader starting at a specific position.
func (rb *RingBuffer[T]) NewReaderAt(pos int64) *RingReader[T] {
	return &RingReader[T]{
		rb:         rb,
		readCursor: pos,
	}
}

// RingReader is a per-consumer cursor into a RingBuffer.
type RingReader[T any] struct {
	rb          *RingBuffer[T]
	readCursor  int64
	lastSkipped int64
	closed      atomic.Bool // per-reader close flag
}

// Read returns the next value, blocking until data is available.
// Returns (value, true) on success, or (zero, false) if the buffer or reader is closed and no data remains.
func (r *RingReader[T]) Read() (T, bool) {
	for {
		val, ok := r.TryRead()
		if ok {
			return val, true
		}
		if r.rb.closed.Load() || r.closed.Load() {
			var zero T
			return zero, false
		}
		// Block until writer signals new data via cond.Broadcast().
		// Using sync.Cond avoids the stale-signal busy-spin that occurred
		// with the previous channel + re-broadcast approach.
		r.rb.mu.Lock()
		for r.readCursor >= r.rb.writeCursor.Load() && !r.rb.closed.Load() && !r.closed.Load() {
			r.rb.cond.Wait()
		}
		r.rb.mu.Unlock()
	}
}

// Close marks this reader as closed, causing any blocking Read() to return (zero, false).
// Safe to call concurrently and multiple times.
func (r *RingReader[T]) Close() {
	r.closed.Store(true)
	r.rb.cond.Broadcast()
}

// Signal returns the notification channel of the underlying ring buffer.
// Useful for select-based consumers that need to multiplex with other channels.
func (r *RingReader[T]) Signal() <-chan struct{} {
	return r.rb.signal
}

// TryRead attempts a non-blocking read. Returns (value, false) if no data available.
func (r *RingReader[T]) TryRead() (T, bool) {
	r.lastSkipped = 0

	wc := r.rb.writeCursor.Load()
	if r.readCursor >= wc {
		var zero T
		return zero, false
	}

	// Check if our position was overwritten (reader too slow)
	oldest := wc - r.rb.size
	if r.readCursor < oldest {
		r.lastSkipped = oldest - r.readCursor
		r.readCursor = oldest
	}

	val := r.rb.buf[r.readCursor%r.rb.size]
	r.readCursor++
	return val, true
}

// Skipped returns the number of frames skipped in the last TryRead call
// due to the reader being too slow (ring buffer overwrite).
func (r *RingReader[T]) Skipped() int64 {
	return r.lastSkipped
}

// Lag returns the fraction of the ring buffer capacity that the reader trails behind the writer.
// Returns a value in [0.0, 1.0] where 1.0 means the reader is about to be overwritten.
func (r *RingReader[T]) Lag() float64 {
	wc := r.rb.writeCursor.Load()
	behind := wc - r.readCursor
	if behind < 0 {
		behind = 0
	}
	if behind > r.rb.size {
		behind = r.rb.size
	}
	return float64(behind) / float64(r.rb.size)
}

// ReadCursor returns the current read position.
func (r *RingReader[T]) ReadCursor() int64 {
	return r.readCursor
}
