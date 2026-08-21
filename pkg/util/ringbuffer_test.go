package util

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestRingBufferWriteRead(t *testing.T) {
	rb := NewRingBuffer[int](4)
	rb.Write(10)
	rb.Write(20)
	rb.Write(30)

	reader := rb.NewReader()
	val, ok := reader.Read()
	if !ok || val != 10 {
		t.Errorf("expected (10, true), got (%v, %v)", val, ok)
	}
	val, ok = reader.Read()
	if !ok || val != 20 {
		t.Errorf("expected (20, true), got (%v, %v)", val, ok)
	}
	val, ok = reader.Read()
	if !ok || val != 30 {
		t.Errorf("expected (30, true), got (%v, %v)", val, ok)
	}
	// No more data
	_, ok = reader.TryRead()
	if ok {
		t.Error("expected no more data")
	}
}

func TestRingBufferOverflow(t *testing.T) {
	rb := NewRingBuffer[int](4)
	// Write 6 items into size-4 buffer — oldest 2 should be overwritten
	for i := range 6 {
		rb.Write(i)
	}
	reader := rb.NewReader()
	// Reader should start from oldest available (2)
	val, ok := reader.Read()
	if !ok || val != 2 {
		t.Errorf("expected (2, true), got (%v, %v)", val, ok)
	}
}

func TestRingBufferMultipleReaders(t *testing.T) {
	rb := NewRingBuffer[int](8)
	rb.Write(1)
	rb.Write(2)
	rb.Write(3)

	r1 := rb.NewReader()
	r2 := rb.NewReader()

	v1, _ := r1.Read()
	v2, _ := r2.Read()
	if v1 != v2 {
		t.Errorf("readers should get same first value: r1=%d, r2=%d", v1, v2)
	}

	// r1 advances, r2 stays
	r1.Read()
	v1, _ = r1.Read()
	v2, _ = r2.Read()
	if v1 != 3 || v2 != 2 {
		t.Errorf("expected r1=3, r2=2, got r1=%d, r2=%d", v1, v2)
	}
}

func TestRingBufferClose(t *testing.T) {
	rb := NewRingBuffer[int](8)
	rb.Write(1)

	r := rb.NewReader()
	val, ok := r.Read()
	if !ok || val != 1 {
		t.Fatalf("expected (1, true), got (%d, %v)", val, ok)
	}

	done := make(chan struct{})
	go func() {
		_, ok := r.Read()
		if ok {
			t.Error("expected Read to return false after Close")
		}
		close(done)
	}()

	time.Sleep(10 * time.Millisecond)
	rb.Close()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Read did not unblock after Close")
	}
}

func TestRingReaderLag(t *testing.T) {
	rb := NewRingBuffer[int](10)

	// Write 8 items into buffer of size 10
	for i := range 8 {
		rb.Write(i)
	}

	// Reader at position 0, writer at 8 → lag = 8/10 = 0.8
	reader := rb.NewReaderAt(0)
	lag := reader.Lag()
	if lag < 0.79 || lag > 0.81 {
		t.Errorf("expected lag ~0.8, got %f", lag)
	}

	// Read 3 items → reader at 3, writer at 8 → lag = 5/10 = 0.5
	for range 3 {
		reader.TryRead()
	}
	lag = reader.Lag()
	if lag < 0.49 || lag > 0.51 {
		t.Errorf("expected lag ~0.5, got %f", lag)
	}

	// Reader caught up to writer → lag = 0
	for range 5 {
		reader.TryRead()
	}
	lag = reader.Lag()
	if lag != 0.0 {
		t.Errorf("expected lag 0.0, got %f", lag)
	}

	// Write enough to overflow: writer at 18, oldest at 8, reader still at 8
	// lag should be clamped to 1.0
	for range 10 {
		rb.Write(99)
	}
	lag = reader.Lag()
	if lag != 1.0 {
		t.Errorf("expected lag 1.0 (clamped), got %f", lag)
	}
}

func TestRingBufferCloseStopsWrite(t *testing.T) {
	rb := NewRingBuffer[int](4)
	rb.Write(1)
	rb.Close()
	rb.Write(2) // should be no-op

	if rb.WriteCursor() != 1 {
		t.Errorf("expected cursor=1 after close, got %d", rb.WriteCursor())
	}
}

func TestRingBufferCloseDrainsBufferedValues(t *testing.T) {
	rb := NewRingBuffer[int](4)
	rb.Write(10)
	rb.Write(20)
	reader := rb.NewReader()
	rb.Close()

	for _, want := range []int{10, 20} {
		got, ok := reader.Read()
		if !ok || got != want {
			t.Fatalf("Read after buffer Close = (%d, %v), want (%d, true)", got, ok, want)
		}
	}
	if _, ok := reader.Read(); ok {
		t.Fatal("Read returned data after draining a closed buffer")
	}
}

func TestRingBufferReadDoesNotSpin(t *testing.T) {
	rb := NewRingBuffer[int](16)
	reader := rb.NewReader()

	// Have the reader consume an item through the Read() blocking path:
	// goroutine blocks on Read(), then writer signals.
	readDone := make(chan int)
	go func() {
		val, ok := reader.Read()
		if !ok {
			t.Error("Read returned false unexpectedly")
			return
		}
		readDone <- val
	}()

	time.Sleep(10 * time.Millisecond) // let goroutine block in Read()
	rb.Write(42)

	select {
	case val := <-readDone:
		if val != 42 {
			t.Fatalf("expected 42, got %d", val)
		}
	case <-time.After(time.Second):
		t.Fatal("Read did not return after Write")
	}

	// Now call Read() again with no new data — it should block properly.
	// Verify by checking it does NOT return within 50ms, then unblock with a write.
	readDone2 := make(chan int)
	go func() {
		val, ok := reader.Read()
		if ok {
			readDone2 <- val
		}
	}()

	select {
	case <-readDone2:
		t.Fatal("Read returned without new data — busy-spin bug present")
	case <-time.After(50 * time.Millisecond):
		// Good: Read is properly blocking
	}

	// Unblock the reader
	rb.Write(99)
	select {
	case val := <-readDone2:
		if val != 99 {
			t.Errorf("expected 99, got %d", val)
		}
	case <-time.After(time.Second):
		t.Fatal("Read did not return after second Write")
	}
}

func TestRingBufferReadMultipleReadersNonSpin(t *testing.T) {
	rb := NewRingBuffer[int](16)
	r1 := rb.NewReader()
	r2 := rb.NewReader()

	// Both readers block on Read(), writer signals once
	done1 := make(chan int)
	done2 := make(chan int)
	go func() { val, _ := r1.Read(); done1 <- val }()
	go func() { val, _ := r2.Read(); done2 <- val }()

	time.Sleep(10 * time.Millisecond)
	rb.Write(7)

	select {
	case v := <-done1:
		if v != 7 {
			t.Errorf("r1 expected 7, got %d", v)
		}
	case <-time.After(time.Second):
		t.Fatal("r1 Read did not return")
	}
	select {
	case v := <-done2:
		if v != 7 {
			t.Errorf("r2 expected 7, got %d", v)
		}
	case <-time.After(time.Second):
		t.Fatal("r2 Read did not return")
	}
}

// TestRingBufferReadBlocksAfterPublisherStops simulates the exact user scenario:
// publisher writes frames, then stops. Reader goroutines should block (near-zero
// CPU), NOT busy-spin. Before the sync.Cond fix, this consumed ~100% CPU per reader.
func TestRingBufferReadBlocksAfterPublisherStops(t *testing.T) {
	rb := NewRingBuffer[int](64)

	// Simulate publisher writing 30fps for 100ms
	for i := range 3 {
		rb.Write(i)
		time.Sleep(33 * time.Millisecond)
	}
	// Publisher stops — no more writes

	reader := rb.NewReaderAt(0)
	// Drain all available data
	for {
		_, ok := reader.TryRead()
		if !ok {
			break
		}
	}

	// Now reader calls Read() — should block, not spin.
	// Verify by checking goroutine doesn't return for 100ms.
	readReturned := make(chan struct{})
	go func() {
		reader.Read()
		close(readReturned)
	}()

	select {
	case <-readReturned:
		t.Fatal("Read returned with no new data — goroutine is not properly blocking")
	case <-time.After(100 * time.Millisecond):
		// Good: goroutine is sleeping in cond.Wait(), zero CPU
	}

	// Clean up: close the ring buffer to unblock the goroutine
	rb.Close()
	select {
	case <-readReturned:
	case <-time.After(time.Second):
		t.Fatal("Read did not unblock after Close")
	}
}

// TestRingReaderClose verifies that closing a reader unblocks its Read()
// without affecting the ring buffer or other readers.
func TestRingReaderClose(t *testing.T) {
	rb := NewRingBuffer[int](16)
	r1 := rb.NewReader()
	r2 := rb.NewReader()
	rb.Write(100)

	r1.Close()
	r1.Close() // Close is idempotent.
	if _, ok := r1.Read(); ok {
		t.Fatal("r1.Read() should return false after reader close")
	}

	// Closing r1 discards its pending value without affecting r2.
	val, ok := r2.Read()
	if !ok || val != 100 {
		t.Fatalf("r2 expected (100, true), got (%d, %v)", val, ok)
	}
}

func TestRingReaderCloseInterruptsBlockedRead(t *testing.T) {
	previousProcs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previousProcs)

	reader := NewRingBuffer[int](16).NewReader()
	started := make(chan struct{})
	result := make(chan bool, 1)
	go func() {
		close(started)
		_, ok := reader.Read()
		result <- ok
	}()

	<-started
	runtime.Gosched()
	reader.Close()

	select {
	case ok := <-result:
		if ok {
			t.Fatal("Read returned data after reader close")
		}
	case <-time.After(time.Second):
		t.Fatal("reader Close did not interrupt blocked Read")
	}
}

func TestRingReaderCloseDoesNotLeaveContextWake(t *testing.T) {
	rb := NewRingBuffer[int](16)
	reader := rb.NewReader()
	oldContext := newTrackedAfterContext()
	reader.ensureContextWake(oldContext)
	<-oldContext.registered

	// Hold the predicate lock so Close pauses after removing the old watcher.
	rb.mu.Lock()
	closeDone := make(chan struct{})
	go func() {
		reader.Close()
		close(closeDone)
	}()
	<-oldContext.stopped

	newContext := newTrackedAfterContext()
	readDone := make(chan struct{})
	go func() {
		reader.ReadContext(newContext)
		close(readDone)
	}()
	runtime.Gosched()
	rb.mu.Unlock()

	select {
	case <-closeDone:
	case <-time.After(time.Second):
		t.Fatal("reader Close did not finish")
	}
	select {
	case <-readDone:
	case <-time.After(time.Second):
		t.Fatal("concurrent ReadContext did not observe reader close")
	}
	select {
	case <-newContext.registered:
		select {
		case <-newContext.stopped:
		case <-time.After(time.Second):
			t.Fatal("reader Close left a newly registered context wake active")
		}
	default:
	}
}

func TestRingReaderReadContextCancellation(t *testing.T) {
	previousProcs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previousProcs)

	rb := NewRingBuffer[int](16)
	reader := rb.NewReader()
	baseCtx, cancel := context.WithCancel(context.Background())
	ctx := newPredicateBarrierContext(baseCtx, 3)

	result := make(chan bool, 1)
	go func() {
		_, ok := reader.ReadContext(ctx)
		result <- ok
	}()

	<-ctx.reached
	close(ctx.release)
	runtime.Gosched()
	cancel()

	select {
	case ok := <-result:
		if ok {
			t.Fatal("ReadContext returned data after cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("ReadContext did not unblock after cancellation")
	}

	if rb.IsClosed() {
		t.Fatal("canceling one reader context closed the ring buffer")
	}

	rb.Write(7)
	if got, ok := reader.ReadContext(context.Background()); !ok || got != 7 {
		t.Fatalf("reader was not reusable after call cancellation: got (%d, %v)", got, ok)
	}
}

func TestRingBufferReadContextDoesNotLoseWakeups(t *testing.T) {
	previousProcs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previousProcs)

	rb := NewRingBuffer[int](1)
	reader := rb.NewReaderAt(0)
	ctx := newPredicateBarrierContext(context.Background(), 3)
	result := make(chan int, 1)
	go func() {
		got, ok := reader.ReadContext(ctx)
		if !ok {
			result <- -1
			return
		}
		result <- got
	}()

	// The reader has evaluated the no-data predicate while holding rb.mu and
	// is paused immediately before cond.Wait. A correct writer must block on
	// the same mutex until the reader registers its wait.
	<-ctx.reached
	writeDone := make(chan struct{})
	go func() {
		rb.Write(42)
		close(writeDone)
	}()
	runtime.Gosched()
	publishedBeforeWait := rb.WriteCursor() != 0
	close(ctx.release)

	if publishedBeforeWait {
		// Let the reader enter Wait, then clean it up before reporting the
		// synchronization violation.
		runtime.Gosched()
		rb.Close()
	}

	select {
	case got := <-result:
		if publishedBeforeWait {
			t.Fatal("writer published between the predicate check and cond.Wait")
		}
		if got != 42 {
			t.Fatalf("read got %d, want 42", got)
		}
	case <-time.After(time.Second):
		t.Fatal("reader lost the write wakeup")
	}
	select {
	case <-writeDone:
	case <-time.After(time.Second):
		t.Fatal("writer did not complete")
	}
}

type predicateBarrierContext struct {
	context.Context
	blockAt int
	calls   int
	reached chan struct{}
	release chan struct{}
}

type trackedAfterContext struct {
	done       chan struct{}
	registered chan struct{}
	stopped    chan struct{}
}

func newTrackedAfterContext() *trackedAfterContext {
	return &trackedAfterContext{
		done:       make(chan struct{}),
		registered: make(chan struct{}),
		stopped:    make(chan struct{}),
	}
}

func (c *trackedAfterContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *trackedAfterContext) Done() <-chan struct{}       { return c.done }
func (c *trackedAfterContext) Err() error                  { return nil }
func (c *trackedAfterContext) Value(any) any               { return nil }

func (c *trackedAfterContext) AfterFunc(func()) func() bool {
	close(c.registered)
	var once sync.Once
	return func() bool {
		stopped := false
		once.Do(func() {
			stopped = true
			close(c.stopped)
		})
		return stopped
	}
}

func newPredicateBarrierContext(parent context.Context, blockAt int) *predicateBarrierContext {
	return &predicateBarrierContext{
		Context: parent,
		blockAt: blockAt,
		reached: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (c *predicateBarrierContext) Err() error {
	c.calls++
	if c.calls == c.blockAt {
		close(c.reached)
		<-c.release
	}
	return c.Context.Err()
}

func BenchmarkRingBufferHighReaderCount(b *testing.B) {
	benchmarkRingBufferHighReaderCount(b, false)
}

func BenchmarkRingBufferReadContextHighReaderCount(b *testing.B) {
	benchmarkRingBufferHighReaderCount(b, true)
}

func benchmarkRingBufferHighReaderCount(b *testing.B, useContext bool) {
	for _, readerCount := range []int{1, 16, 128} {
		b.Run(fmt.Sprintf("readers-%d", readerCount), func(b *testing.B) {
			rb := NewRingBuffer[int](b.N)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			start := make(chan struct{})
			readers := make([]*RingReader[int], 0, readerCount)
			var done sync.WaitGroup
			done.Add(readerCount)

			for range readerCount {
				reader := rb.NewReaderAt(0)
				readers = append(readers, reader)
				go func() {
					defer done.Done()
					defer reader.Close()
					<-start
					for range b.N {
						var ok bool
						if useContext {
							_, ok = reader.ReadContext(ctx)
						} else {
							_, ok = reader.Read()
						}
						if !ok {
							return
						}
					}
				}()
			}

			close(start)
			if useContext {
				waitForContextRegistration(b, readers, ctx.Done())
			}
			b.ResetTimer()
			for value := range b.N {
				rb.Write(value)
			}
			done.Wait()
			b.StopTimer()
		})
	}
}

func waitForContextRegistration(b *testing.B, readers []*RingReader[int], done <-chan struct{}) {
	b.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		allRegistered := true
		for _, reader := range readers {
			reader.contextMu.Lock()
			registered := reader.contextDone == done && reader.stopContext != nil
			reader.contextMu.Unlock()
			if !registered {
				allRegistered = false
				break
			}
		}
		if allRegistered {
			return
		}
		if time.Now().After(deadline) {
			b.Fatal("readers did not register their context wake")
		}
		runtime.Gosched()
	}
}
