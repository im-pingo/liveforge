package util

import (
	"sync/atomic"
)

// RingBuffer is a generic single-producer, multi-consumer ring buffer.
// The writer advances atomically; each reader maintains its own cursor.
type RingBuffer[T any] struct {
	buf         []T
	size        int64
	writeCursor atomic.Int64 // next write position (monotonically increasing)
	signal      chan struct{}
}

// NewRingBuffer creates a new ring buffer with the given capacity.
func NewRingBuffer[T any](size int) *RingBuffer[T] {
	return &RingBuffer[T]{
		buf:    make([]T, size),
		size:   int64(size),
		signal: make(chan struct{}, 1),
	}
}

// Write adds a value to the ring buffer. If the buffer is full, the oldest
// value is silently overwritten.
func (rb *RingBuffer[T]) Write(val T) {
	pos := rb.writeCursor.Add(1) - 1
	rb.buf[pos%rb.size] = val

	// Non-blocking notify to wake any waiting readers
	select {
	case rb.signal <- struct{}{}:
	default:
	}
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
	rb         *RingBuffer[T]
	readCursor int64
}

// Read returns the next value, blocking until data is available.
// Returns (value, true) on success, or (zero, false) if the reader is closed.
func (r *RingReader[T]) Read() (T, bool) {
	for {
		val, ok := r.TryRead()
		if ok {
			return val, true
		}
		// Wait for signal from writer
		<-r.rb.signal
		// Re-broadcast signal for other waiting readers
		select {
		case r.rb.signal <- struct{}{}:
		default:
		}
	}
}

// TryRead attempts a non-blocking read. Returns (value, false) if no data available.
func (r *RingReader[T]) TryRead() (T, bool) {
	wc := r.rb.writeCursor.Load()
	if r.readCursor >= wc {
		var zero T
		return zero, false
	}

	// Check if our position was overwritten (reader too slow)
	oldest := wc - r.rb.size
	if r.readCursor < oldest {
		r.readCursor = oldest
	}

	val := r.rb.buf[r.readCursor%r.rb.size]
	r.readCursor++
	return val, true
}

// ReadCursor returns the current read position.
func (r *RingReader[T]) ReadCursor() int64 {
	return r.readCursor
}
