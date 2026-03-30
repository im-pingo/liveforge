package util

import (
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
