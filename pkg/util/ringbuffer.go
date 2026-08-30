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
	signal      chan struct{}
	closed      atomic.Bool
	mu          sync.Mutex   // protects cond for Read() blocking
	cond        *sync.Cond   // wakes blocked Read() callers on Write/Close
	dataMu      sync.RWMutex // protects buf slot access against concurrent read/write
	testHooks   *ringBufferTestHooks
}

type ringBufferTestHooks struct {
	beforeReadSlotLock     func()
	afterAdvanceCapture    func()
	writeSlotLockAttempted func(bool)
}

// RingReadResult binds overwrite metadata to the value returned by one read.
type RingReadResult[T any] struct {
	Value       T
	OK          bool
	Overwritten int64
}

// NewRingBuffer creates a new ring buffer with the given capacity.
func NewRingBuffer[T any](size int) *RingBuffer[T] {
	if size <= 0 {
		// Keep the low-level container safe for direct callers. Configuration
		// validation still rejects this value so production streams fail closed.
		size = 1
	}
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
	// Slot store and cursor advance happen under the same lock so readers
	// holding the read lock always see a cursor consistent with slot
	// contents (otherwise a reader could fetch a just-overwritten slot
	// before the cursor reveals the overwrite, breaking frame ordering).
	pos := rb.writeCursor.Load()
	if hooks := rb.testHooks; hooks != nil && hooks.writeSlotLockAttempted != nil {
		if rb.dataMu.TryLock() {
			hooks.writeSlotLockAttempted(false)
		} else {
			hooks.writeSlotLockAttempted(true)
			rb.dataMu.Lock()
		}
	} else {
		rb.dataMu.Lock()
	}
	rb.buf[pos%rb.size] = val
	rb.writeCursor.Store(pos + 1)
	rb.dataMu.Unlock()

	// Wake all Read() callers blocked on cond.Wait(). The mutex closes the
	// check-then-wait window so a writer cannot signal before a waiter is
	// registered with sync.Cond.
	rb.broadcast()

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
	// Wake all Read() callers blocked on cond.Wait().
	rb.broadcast()
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

func (rb *RingBuffer[T]) broadcast() {
	rb.mu.Lock()
	rb.cond.Broadcast()
	rb.mu.Unlock()
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
	return newRingReader(rb, start)
}

// NewReaderAt creates a new reader starting at a specific position.
func (rb *RingBuffer[T]) NewReaderAt(pos int64) *RingReader[T] {
	return newRingReader(rb, pos)
}

func newRingReader[T any](rb *RingBuffer[T], pos int64) *RingReader[T] {
	r := &RingReader[T]{rb: rb}
	r.readCursor.Store(pos)
	return r
}

// RingReader is a per-consumer cursor into a RingBuffer.
type RingReader[T any] struct {
	rb          *RingBuffer[T]
	readCursor  atomic.Int64
	lastSkipped atomic.Int64
	closed      atomic.Bool // per-reader close flag
	contextMu   sync.Mutex
	contextDone <-chan struct{}
	stopContext func() bool
}

// Read returns the next value, blocking until data is available.
// Returns (value, true) on success, or (zero, false) if the buffer or reader is closed and no data remains.
func (r *RingReader[T]) Read() (T, bool) {
	result := r.ReadResult()
	return result.Value, result.OK
}

// ReadContext returns the next value, blocking until data is available, the
// reader or buffer is closed, or ctx is cancelled. Unlike Signal, the wait is
// scoped to this reader and cannot be consumed by another consumer.
func (r *RingReader[T]) ReadContext(ctx context.Context) (T, bool) {
	result := r.ReadResultContext(ctx)
	return result.Value, result.OK
}

// ReadResult returns the next value and its overwrite metadata, blocking until
// data is available or the buffer or reader is closed.
func (r *RingReader[T]) ReadResult() RingReadResult[T] {
	return r.readResultContext(context.Background())
}

// ReadResultContext returns the next value and its overwrite metadata,
// blocking until data is available, the reader or buffer is closed, or ctx is
// cancelled.
func (r *RingReader[T]) ReadResultContext(ctx context.Context) RingReadResult[T] {
	if ctx == nil {
		panic("nil context")
	}
	return r.readResultContext(ctx)
}

func (r *RingReader[T]) readResultContext(ctx context.Context) RingReadResult[T] {
	if r.closed.Load() || contextCanceled(ctx) {
		return RingReadResult[T]{}
	}
	if result := r.TryReadResult(); result.OK {
		return result
	}
	r.ensureContextWake(ctx)

	for {
		if r.closed.Load() || contextCanceled(ctx) {
			return RingReadResult[T]{}
		}
		r.rb.mu.Lock()
		for r.readCursor.Load() >= r.rb.writeCursor.Load() &&
			!r.rb.closed.Load() && !r.closed.Load() && !contextCanceled(ctx) {
			r.rb.cond.Wait()
		}
		r.rb.mu.Unlock()

		if contextCanceled(ctx) {
			return RingReadResult[T]{}
		}
		if result := r.TryReadResult(); result.OK {
			return result
		}
		if r.rb.closed.Load() || r.closed.Load() {
			return RingReadResult[T]{}
		}
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
	if r.closed.Load() || done == r.contextDone {
		return
	}
	if r.stopContext != nil {
		r.stopContext()
		r.stopContext = nil
	}
	r.contextDone = done
	if done != nil {
		r.stopContext = context.AfterFunc(ctx, func() {
			r.rb.broadcast()
		})
	}
}

// WaitContext blocks until this reader has unread data, the reader or buffer
// is closed, or ctx is cancelled. It does not consume a value.
func (r *RingReader[T]) WaitContext(ctx context.Context) bool {
	if ctx == nil {
		ctx = context.Background()
	}
	if r.closed.Load() || ctx.Err() != nil {
		return false
	}
	if r.readCursor.Load() < r.rb.writeCursor.Load() {
		return true
	}
	if r.rb.closed.Load() {
		return false
	}

	r.ensureContextWake(ctx)

	r.rb.mu.Lock()
	for r.readCursor.Load() >= r.rb.writeCursor.Load() &&
		!r.rb.closed.Load() && !r.closed.Load() && ctx.Err() == nil {
		r.rb.cond.Wait()
	}
	ready := r.readCursor.Load() < r.rb.writeCursor.Load() &&
		!r.closed.Load() && ctx.Err() == nil
	r.rb.mu.Unlock()
	return ready
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

// Signal returns the notification channel of the underlying ring buffer.
// Useful for select-based consumers that need to multiplex with other channels.
func (r *RingReader[T]) Signal() <-chan struct{} {
	return r.rb.signal
}

// TryRead attempts a non-blocking read. Returns (value, false) if no data available.
func (r *RingReader[T]) TryRead() (T, bool) {
	result := r.TryReadResult()
	return result.Value, result.OK
}

// TryReadResult attempts a non-blocking read and returns overwrite metadata
// from the same operation as the value.
func (r *RingReader[T]) TryReadResult() RingReadResult[T] {
	r.lastSkipped.Store(0)
	var result RingReadResult[T]

	for {
		wc := r.rb.writeCursor.Load()
		readCursor := r.readCursor.Load()
		if readCursor >= wc {
			return result
		}

		// Check if our position was overwritten (reader too slow)
		oldest := wc - r.rb.size
		if readCursor < oldest {
			result.Overwritten += oldest - readCursor
			readCursor = oldest
			r.readCursor.Store(readCursor)
		}

		if hooks := r.rb.testHooks; hooks != nil && hooks.beforeReadSlotLock != nil {
			hooks.beforeReadSlotLock()
		}
		r.rb.dataMu.RLock()
		val := r.rb.buf[readCursor%r.rb.size]
		// Re-check under the lock: if the writer lapped us between loading
		// the cursor and acquiring the lock, the slot now holds a newer
		// frame and returning it would break ordering. Retry from the new
		// oldest position instead.
		lapped := readCursor < r.rb.writeCursor.Load()-r.rb.size
		r.rb.dataMu.RUnlock()
		if lapped {
			continue
		}

		r.readCursor.Store(readCursor + 1)
		result.Value = val
		result.OK = true
		r.lastSkipped.Store(result.Overwritten)
		return result
	}
}

// Skipped returns the number of frames skipped in the last TryRead call
// due to the reader being too slow (ring buffer overwrite).
func (r *RingReader[T]) Skipped() int64 {
	return r.lastSkipped.Load()
}

// AdvanceToLive discards unread positions through a captured write cursor.
// Values written after that cursor remain available to the reader.
func (r *RingReader[T]) AdvanceToLive() int64 {
	r.rb.dataMu.RLock()
	defer r.rb.dataMu.RUnlock()

	writeCursor := r.rb.writeCursor.Load()
	if hooks := r.rb.testHooks; hooks != nil && hooks.afterAdvanceCapture != nil {
		hooks.afterAdvanceCapture()
	}
	readCursor := r.readCursor.Load()
	if readCursor >= writeCursor {
		return 0
	}
	r.readCursor.Store(writeCursor)
	return writeCursor - readCursor
}

// Lag returns the fraction of the ring buffer capacity that the reader trails behind the writer.
// Returns a value in [0.0, 1.0] where 1.0 means the reader is about to be overwritten.
func (r *RingReader[T]) Lag() float64 {
	wc := r.rb.writeCursor.Load()
	behind := wc - r.readCursor.Load()
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
	return r.readCursor.Load()
}
