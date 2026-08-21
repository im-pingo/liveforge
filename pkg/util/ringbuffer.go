package util

import (
	"context"
	"sync"
	"sync/atomic"
)

// RingBuffer is a generic single-producer, multi-consumer ring buffer.
// The writer advances atomically; each reader maintains its own cursor.
type RingBuffer[T any] struct {
	buf         []T
	size        int64
	writeCursor atomic.Int64 // next write position (monotonically increasing)
	closed      atomic.Bool
	mu          sync.Mutex   // protects cond for Read() blocking
	cond        *sync.Cond   // wakes blocked Read() callers on Write/Close
	dataMu      sync.RWMutex // protects buf slot access against concurrent read/write
}

// NewRingBuffer creates a new ring buffer with the given capacity.
func NewRingBuffer[T any](size int) *RingBuffer[T] {
	rb := &RingBuffer[T]{
		buf:  make([]T, size),
		size: int64(size),
	}
	rb.cond = sync.NewCond(&rb.mu)
	return rb
}

// Write adds a value to the ring buffer. If the buffer is full, the oldest
// value is silently overwritten. No-op if the buffer is closed.
func (rb *RingBuffer[T]) Write(val T) {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	if rb.closed.Load() {
		return
	}
	// Slot store and cursor advance happen under the same lock so readers
	// holding the read lock always see a cursor consistent with slot
	// contents (otherwise a reader could fetch a just-overwritten slot
	// before the cursor reveals the overwrite, breaking frame ordering).
	pos := rb.writeCursor.Load()
	rb.dataMu.Lock()
	rb.buf[pos%rb.size] = val
	rb.writeCursor.Store(pos + 1)
	rb.dataMu.Unlock()

	// The predicate update and notification share rb.mu so a reader cannot
	// observe no data and then miss this wakeup before entering cond.Wait.
	rb.cond.Broadcast()
}

// Close signals all blocked readers to return (zero, false).
// After Close, Write is a no-op.
func (rb *RingBuffer[T]) Close() {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	rb.closed.Store(true)
	rb.cond.Broadcast()
}

// IsClosed returns whether the ring buffer has been closed.
func (rb *RingBuffer[T]) IsClosed() bool {
	return rb.closed.Load()
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
	contextMu   sync.Mutex
	contextDone <-chan struct{}
	stopContext func() bool
}

// Read returns the next value, blocking until data is available.
// Returns (value, true) on success. Closing the reader stops reads immediately;
// closing the buffer allows already-buffered values to be drained first.
func (r *RingReader[T]) Read() (T, bool) {
	return r.readContext(nil)
}

// ReadContext returns the next value, blocking until data is available or ctx
// is canceled. Cancellation affects only this call and does not close the
// reader or its ring buffer.
func (r *RingReader[T]) ReadContext(ctx context.Context) (T, bool) {
	if ctx == nil {
		panic("nil context")
	}
	return r.readContext(ctx)
}

func (r *RingReader[T]) readContext(ctx context.Context) (T, bool) {
	if r.closed.Load() || contextCanceled(ctx) {
		var zero T
		return zero, false
	}

	if val, ok := r.TryRead(); ok {
		return val, true
	}

	r.ensureContextWake(ctx)

	for {
		if r.closed.Load() || contextCanceled(ctx) {
			var zero T
			return zero, false
		}
		if val, ok := r.TryRead(); ok {
			return val, true
		}
		if r.rb.closed.Load() || r.closed.Load() {
			var zero T
			return zero, false
		}
		r.rb.mu.Lock()
		for r.readCursor >= r.rb.writeCursor.Load() &&
			!r.rb.closed.Load() && !r.closed.Load() && !contextCanceled(ctx) {
			r.rb.cond.Wait()
		}
		r.rb.mu.Unlock()
	}
}

func contextCanceled(ctx context.Context) bool {
	return ctx != nil && ctx.Err() != nil
}

func (r *RingReader[T]) ensureContextWake(ctx context.Context) {
	var done <-chan struct{}
	if ctx != nil {
		done = ctx.Done()
	}

	r.contextMu.Lock()
	defer r.contextMu.Unlock()
	if r.closed.Load() {
		return
	}
	if done == r.contextDone {
		return
	}
	if r.stopContext != nil {
		r.stopContext()
		r.stopContext = nil
	}
	r.contextDone = done
	if done != nil {
		r.stopContext = context.AfterFunc(ctx, func() {
			r.rb.mu.Lock()
			r.rb.cond.Broadcast()
			r.rb.mu.Unlock()
		})
	}
}

// Close marks this reader as closed, causing any blocking Read() to return (zero, false).
// Safe to call concurrently and multiple times.
func (r *RingReader[T]) Close() {
	r.contextMu.Lock()
	defer r.contextMu.Unlock()
	if r.stopContext != nil {
		r.stopContext()
		r.stopContext = nil
	}
	r.contextDone = nil

	r.rb.mu.Lock()
	defer r.rb.mu.Unlock()

	r.closed.Store(true)
	r.rb.cond.Broadcast()
}

// TryRead attempts a non-blocking read. Returns (value, false) if no data available.
func (r *RingReader[T]) TryRead() (T, bool) {
	if r.closed.Load() {
		var zero T
		return zero, false
	}

	r.lastSkipped = 0

	for {
		wc := r.rb.writeCursor.Load()
		if r.readCursor >= wc {
			var zero T
			return zero, false
		}

		// Check if our position was overwritten (reader too slow)
		oldest := wc - r.rb.size
		if r.readCursor < oldest {
			r.lastSkipped += oldest - r.readCursor
			r.readCursor = oldest
		}

		r.rb.dataMu.RLock()
		val := r.rb.buf[r.readCursor%r.rb.size]
		// Re-check under the lock: if the writer lapped us between loading
		// the cursor and acquiring the lock, the slot now holds a newer
		// frame and returning it would break ordering. Retry from the new
		// oldest position instead.
		lapped := r.readCursor < r.rb.writeCursor.Load()-r.rb.size
		r.rb.dataMu.RUnlock()
		if lapped {
			continue
		}

		r.readCursor++
		return val, true
	}
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
