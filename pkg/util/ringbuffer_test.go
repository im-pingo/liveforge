package util

import (
	"context"
	"strconv"
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

func TestRingBufferInvalidCapacityFallsBackToOne(t *testing.T) {
	for _, size := range []int{0, -1} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			rb := NewRingBuffer[int](size)
			rb.Write(42)
			reader := rb.NewReader()
			if got, ok := reader.TryRead(); !ok || got != 42 {
				t.Fatalf("capacity %d read = (%d, %v), want (42, true)", size, got, ok)
			}
			if reader.Lag() != 0 {
				t.Fatalf("capacity %d lag = %v, want zero", size, reader.Lag())
			}
		})
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

func TestRingReaderReadReportsOverwriteAtomically(t *testing.T) {
	t.Run("retry-accumulation", func(t *testing.T) {
		rb := NewRingBuffer[int](2)
		reader := rb.NewReaderAt(0)
		for _, value := range []int{10, 20, 30, 40} {
			rb.Write(value)
		}

		firstSlotAttempt := make(chan struct{})
		resumeRead := make(chan struct{})
		hookCalls := 0
		rb.testHooks = &ringBufferTestHooks{
			beforeReadSlotLock: func() {
				hookCalls++
				if hookCalls == 1 {
					close(firstSlotAttempt)
					<-resumeRead
				}
			},
		}

		result := make(chan RingReadResult[int], 1)
		go func() {
			result <- reader.TryReadResult()
		}()

		select {
		case <-firstSlotAttempt:
		case <-time.After(time.Second):
			t.Fatal("TryReadResult did not reach the first slot acquisition boundary")
		}
		rb.Write(50)
		rb.Write(60)
		close(resumeRead)

		var got RingReadResult[int]
		select {
		case got = <-result:
		case <-time.After(time.Second):
			t.Fatal("TryReadResult did not complete after retry release")
		}
		if hookCalls != 2 {
			t.Fatalf("slot acquisition attempts = %d, want 2", hookCalls)
		}
		if !got.OK || got.Value != 50 || got.Overwritten != 4 {
			t.Fatalf("TryReadResult = %+v, want {Value:50 OK:true Overwritten:4}", got)
		}

		got = reader.TryReadResult()
		if !got.OK || got.Value != 60 || got.Overwritten != 0 {
			t.Fatalf("next TryReadResult = %+v, want {Value:60 OK:true Overwritten:0}", got)
		}
	})

	t.Run("blocking", func(t *testing.T) {
		rb := NewRingBuffer[int](2)
		reader := rb.NewReaderAt(0)
		for _, value := range []int{10, 20, 30, 40} {
			rb.Write(value)
		}

		got := reader.ReadResult()
		if !got.OK || got.Value != 30 || got.Overwritten != 2 {
			t.Fatalf("ReadResult = %+v, want {Value:30 OK:true Overwritten:2}", got)
		}

		got = reader.ReadResult()
		if !got.OK || got.Value != 40 || got.Overwritten != 0 {
			t.Fatalf("next ReadResult = %+v, want {Value:40 OK:true Overwritten:0}", got)
		}
	})

	t.Run("context", func(t *testing.T) {
		rb := NewRingBuffer[int](2)
		reader := rb.NewReaderAt(0)
		for _, value := range []int{10, 20, 30, 40} {
			rb.Write(value)
		}

		got := reader.ReadResultContext(context.Background())
		if !got.OK || got.Value != 30 || got.Overwritten != 2 {
			t.Fatalf("ReadResultContext = %+v, want {Value:30 OK:true Overwritten:2}", got)
		}

		got = reader.ReadResultContext(context.Background())
		if !got.OK || got.Value != 40 || got.Overwritten != 0 {
			t.Fatalf("next ReadResultContext = %+v, want {Value:40 OK:true Overwritten:0}", got)
		}
	})
}

func TestRingReaderLegacySkippedCompatibility(t *testing.T) {
	legacyReads := []struct {
		name string
		read func(*RingReader[int]) (int, bool)
	}{
		{name: "TryRead", read: func(reader *RingReader[int]) (int, bool) {
			return reader.TryRead()
		}},
		{name: "Read", read: func(reader *RingReader[int]) (int, bool) {
			return reader.Read()
		}},
		{name: "ReadContext", read: func(reader *RingReader[int]) (int, bool) {
			return reader.ReadContext(context.Background())
		}},
	}

	for _, test := range legacyReads {
		t.Run(test.name, func(t *testing.T) {
			rb := NewRingBuffer[int](2)
			reader := rb.NewReaderAt(0)
			for _, value := range []int{10, 20, 30, 40} {
				rb.Write(value)
			}

			value, ok := test.read(reader)
			if !ok || value != 30 {
				t.Fatalf("first legacy read = (%d, %v), want (30, true)", value, ok)
			}
			if skipped := reader.Skipped(); skipped != 2 {
				t.Fatalf("Skipped after overwrite = %d, want 2", skipped)
			}

			value, ok = test.read(reader)
			if !ok || value != 40 {
				t.Fatalf("next legacy read = (%d, %v), want (40, true)", value, ok)
			}
			if skipped := reader.Skipped(); skipped != 0 {
				t.Fatalf("Skipped after continuous read = %d, want 0", skipped)
			}
		})
	}

	t.Run("pre-cancel-preserves-nonzero", func(t *testing.T) {
		rb := NewRingBuffer[int](2)
		reader := rb.NewReaderAt(0)
		for _, value := range []int{10, 20, 30, 40} {
			rb.Write(value)
		}
		if value, ok := reader.TryRead(); !ok || value != 30 {
			t.Fatalf("seed TryRead = (%d, %v), want (30, true)", value, ok)
		}
		if skipped := reader.Skipped(); skipped != 2 {
			t.Fatalf("seed Skipped = %d, want 2", skipped)
		}
		cursor := reader.ReadCursor()

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, ok := reader.ReadContext(ctx); ok {
			t.Fatal("pre-cancelled ReadContext returned a value")
		}
		if skipped := reader.Skipped(); skipped != 2 {
			t.Fatalf("Skipped after pre-cancelled read = %d, want preserved value 2", skipped)
		}
		if got := reader.ReadCursor(); got != cursor {
			t.Fatalf("cursor after pre-cancelled read = %d, want %d", got, cursor)
		}
	})
}

func TestRingReaderSkippedConcurrentObserver(t *testing.T) {
	rb := NewRingBuffer[int](1)
	reader := rb.NewReaderAt(0)
	rb.Write(0)
	rb.Write(1)
	if _, ok := reader.TryRead(); !ok || reader.Skipped() != 1 {
		t.Fatal("failed to seed a nonzero compatibility skip")
	}

	const iterations = 2000
	start := make(chan struct{})
	readDone := make(chan bool, 1)
	observeDone := make(chan int64, 1)
	go func() {
		<-start
		for i := range iterations {
			rb.Write(i*2 + 2)
			rb.Write(i*2 + 3)
			if _, ok := reader.TryRead(); !ok {
				readDone <- false
				return
			}
		}
		readDone <- true
	}()
	go func() {
		<-start
		var observed int64
		for range iterations {
			observed += reader.Skipped()
		}
		observeDone <- observed
	}()
	close(start)

	select {
	case ok := <-readDone:
		if !ok {
			t.Fatal("legacy reader unexpectedly ran out of data")
		}
	case <-time.After(time.Second):
		t.Fatal("legacy reader did not complete")
	}
	select {
	case observed := <-observeDone:
		if observed <= 0 {
			t.Fatalf("concurrent observer sum = %d, want positive", observed)
		}
	case <-time.After(time.Second):
		t.Fatal("Skipped observer did not complete")
	}
}

func TestRingReaderAdvanceToLivePreservesLaterWrites(t *testing.T) {
	rb := NewRingBuffer[int](4)
	reader := rb.NewReaderAt(0)
	for _, value := range []int{10, 20, 30} {
		rb.Write(value)
	}

	captured := make(chan struct{})
	resumeAdvance := make(chan struct{})
	writeSlotLockAttempted := make(chan bool, 1)
	rb.testHooks = &ringBufferTestHooks{
		afterAdvanceCapture: func() {
			close(captured)
			<-resumeAdvance
		},
		writeSlotLockAttempted: func(contended bool) {
			writeSlotLockAttempted <- contended
		},
	}
	advanceDone := make(chan int64, 1)
	go func() {
		advanceDone <- reader.AdvanceToLive()
	}()

	select {
	case <-captured:
	case <-time.After(time.Second):
		t.Fatal("AdvanceToLive did not reach the capture boundary")
	}

	writerDone := make(chan struct{})
	go func() {
		rb.Write(40)
		close(writerDone)
	}()

	var writerContended bool
	select {
	case writerContended = <-writeSlotLockAttempted:
	case <-time.After(time.Second):
		close(resumeAdvance)
		cleanupTimer := time.NewTimer(time.Second)
		defer cleanupTimer.Stop()
		for advanceDone != nil || writerDone != nil {
			select {
			case <-advanceDone:
				advanceDone = nil
			case <-writerDone:
				writerDone = nil
			case <-cleanupTimer.C:
				t.Fatal("writer did not attempt the slot lock at the capture boundary; cleanup did not complete")
			}
		}
		t.Fatal("writer did not attempt the slot lock at the capture boundary")
	}
	close(resumeAdvance)

	var discarded int64
	select {
	case discarded = <-advanceDone:
	case <-time.After(time.Second):
		t.Fatal("AdvanceToLive did not complete after capture release")
	}
	if discarded != 3 {
		t.Fatalf("AdvanceToLive discarded = %d, want captured count 3", discarded)
	}
	select {
	case <-writerDone:
	case <-time.After(time.Second):
		t.Fatal("writer did not complete after AdvanceToLive released the slot lock")
	}
	if !writerContended {
		t.Fatal("writer slot lock attempt did not contend with AdvanceToLive capture")
	}
	got := reader.TryReadResult()
	if !got.OK || got.Value != 40 || got.Overwritten != 0 {
		t.Fatalf("TryReadResult after later write = %+v, want {Value:40 OK:true Overwritten:0}", got)
	}
}

func TestRingReaderReadResultContextCancellationDoesNotAdvance(t *testing.T) {
	rb := NewRingBuffer[int](2)
	reader := rb.NewReaderAt(0)
	for _, value := range []int{10, 20, 30} {
		rb.Write(value)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got := reader.ReadResultContext(ctx)
	if got.OK || got.Value != 0 || got.Overwritten != 0 {
		t.Fatalf("cancelled ReadResultContext = %+v, want zero unavailable result", got)
	}
	if cursor := reader.ReadCursor(); cursor != 0 {
		t.Fatalf("reader cursor after cancellation = %d, want 0", cursor)
	}
	if skipped := reader.Skipped(); skipped != 0 {
		t.Fatalf("Skipped after cancellation = %d, want 0", skipped)
	}
}

func TestRingReaderReadResultEmptyAndClosed(t *testing.T) {
	rb := NewRingBuffer[int](2)
	reader := rb.NewReader()

	if got := reader.TryReadResult(); got.OK || got.Value != 0 || got.Overwritten != 0 {
		t.Fatalf("empty TryReadResult = %+v, want zero unavailable result", got)
	}
	rb.Close()
	if got := reader.ReadResult(); got.OK || got.Value != 0 || got.Overwritten != 0 {
		t.Fatalf("closed ReadResult = %+v, want zero unavailable result", got)
	}
}

func TestRingReaderReadResultAllocations(t *testing.T) {
	rb := NewRingBuffer[int](8)
	reader := rb.NewReader()
	value := 0
	allocs := testing.AllocsPerRun(1000, func() {
		value++
		rb.Write(value)
		if got := reader.TryReadResult(); !got.OK {
			t.Fatal("TryReadResult returned no value")
		}
	})
	if allocs != 0 {
		t.Fatalf("TryReadResult allocations = %v, want 0", allocs)
	}

	ctx := context.Background()
	allocs = testing.AllocsPerRun(1000, func() {
		value++
		rb.Write(value)
		if got := reader.ReadResultContext(ctx); !got.OK {
			t.Fatal("ReadResultContext returned no value")
		}
	})
	if allocs != 0 {
		t.Fatalf("ReadResultContext immediate allocations = %v, want 0", allocs)
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

func TestRingReaderReadContextWakesIndependentReaders(t *testing.T) {
	rb := NewRingBuffer[int](8)
	r1 := rb.NewReader()
	r2 := rb.NewReader()

	type result struct {
		value int
		ok    bool
	}
	done := make(chan result, 2)
	ctx := context.Background()
	go func() {
		value, ok := r1.ReadContext(ctx)
		done <- result{value: value, ok: ok}
	}()
	go func() {
		value, ok := r2.ReadContext(ctx)
		done <- result{value: value, ok: ok}
	}()

	time.Sleep(10 * time.Millisecond)
	rb.Write(7)

	for range 2 {
		select {
		case got := <-done:
			if !got.ok || got.value != 7 {
				t.Fatalf("ReadContext result = (%d, %v), want (7, true)", got.value, got.ok)
			}
		case <-time.After(time.Second):
			t.Fatal("reader did not wake after Write")
		}
	}
}

func TestRingReaderReadContextCancellation(t *testing.T) {
	rb := NewRingBuffer[int](8)
	reader := rb.NewReader()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() {
		_, ok := reader.ReadContext(ctx)
		done <- ok
	}()

	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("ReadContext returned a frame after cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("ReadContext did not unblock after cancellation")
	}
}

func TestRingReaderReadContextDoesNotConsumeAfterCancellation(t *testing.T) {
	rb := NewRingBuffer[int](8)
	reader := rb.NewReader()
	rb.Write(42)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, ok := reader.ReadContext(ctx); ok {
		t.Fatal("ReadContext returned buffered data after cancellation")
	}
	if cursor := reader.ReadCursor(); cursor != 0 {
		t.Fatalf("reader cursor advanced after cancellation: %d", cursor)
	}
}

func TestRingReaderReadContextDrainsBufferedFramesAfterRingClose(t *testing.T) {
	rb := NewRingBuffer[int](8)
	reader := rb.NewReader()
	rb.Write(42)
	rb.Close()

	if value, ok := reader.ReadContext(context.Background()); !ok || value != 42 {
		t.Fatalf("ReadContext after close = (%d, %v), want (42, true)", value, ok)
	}
	if _, ok := reader.ReadContext(context.Background()); ok {
		t.Fatal("ReadContext returned data after closed buffer was drained")
	}
}

func BenchmarkRingReaderTryRead(b *testing.B) {
	rb := NewRingBuffer[int](1024)
	reader := rb.NewReader()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rb.Write(i)
		if _, ok := reader.TryRead(); !ok {
			b.Fatal("TryRead returned no frame")
		}
	}
}

func BenchmarkRingReaderTryReadResult(b *testing.B) {
	rb := NewRingBuffer[int](1024)
	reader := rb.NewReader()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rb.Write(i)
		if result := reader.TryReadResult(); !result.OK {
			b.Fatal("TryReadResult returned no frame")
		}
	}
}

func BenchmarkRingReaderReadContextImmediate(b *testing.B) {
	rb := NewRingBuffer[int](1024)
	reader := rb.NewReader()
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rb.Write(i)
		if _, ok := reader.ReadContext(ctx); !ok {
			b.Fatal("ReadContext returned no frame")
		}
	}
}

func BenchmarkRingReaderReadContextImmediateResult(b *testing.B) {
	rb := NewRingBuffer[int](1024)
	reader := rb.NewReader()
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rb.Write(i)
		if result := reader.ReadResultContext(ctx); !result.OK {
			b.Fatal("ReadResultContext returned no frame")
		}
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

	done1 := make(chan struct{})
	done2 := make(chan struct{})
	go func() {
		_, ok := r1.Read()
		if ok {
			t.Error("r1.Read() should return false after reader close")
		}
		close(done1)
	}()
	go func() {
		val, ok := r2.Read()
		if !ok || val != 100 {
			t.Errorf("r2 expected (100, true), got (%d, %v)", val, ok)
		}
		close(done2)
	}()

	time.Sleep(20 * time.Millisecond)

	// Close only r1 — r2 should remain blocked
	r1.Close()

	select {
	case <-done1:
	case <-time.After(time.Second):
		t.Fatal("r1.Read() did not unblock after reader Close()")
	}

	select {
	case <-done2:
		t.Fatal("r2.Read() should still be blocking")
	case <-time.After(50 * time.Millisecond):
		// Good: r2 is unaffected
	}

	// Now unblock r2 with a write
	rb.Write(100)
	select {
	case <-done2:
	case <-time.After(time.Second):
		t.Fatal("r2.Read() did not unblock after Write")
	}
}
