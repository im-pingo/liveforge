package core

import (
	"bytes"
	"testing"
	"time"
)

func TestSharedBufferWriteRead(t *testing.T) {
	sb := NewSharedBuffer(64)

	sb.Write([]byte{1, 2, 3})
	sb.Write([]byte{4, 5, 6})

	r := sb.NewReader()
	data, ok := r.Read()
	if !ok || !bytes.Equal(data, []byte{1, 2, 3}) {
		t.Errorf("expected [1,2,3], got %v (ok=%v)", data, ok)
	}
	data, ok = r.Read()
	if !ok || !bytes.Equal(data, []byte{4, 5, 6}) {
		t.Errorf("expected [4,5,6], got %v (ok=%v)", data, ok)
	}
}

func TestSharedBufferMultipleReaders(t *testing.T) {
	sb := NewSharedBuffer(64)

	sb.Write([]byte{10})
	sb.Write([]byte{20})

	r1 := sb.NewReader()
	r2 := sb.NewReader()

	d1, _ := r1.Read()
	d2, _ := r2.Read()
	if !bytes.Equal(d1, d2) {
		t.Errorf("readers should get same data: r1=%v, r2=%v", d1, d2)
	}
}

func TestSharedBufferOverflow(t *testing.T) {
	sb := NewSharedBuffer(4)

	// Write 6 items into size-4 buffer
	for i := range 6 {
		sb.Write([]byte{byte(i)})
	}

	r := sb.NewReader()
	// Should start from oldest available
	data, ok := r.Read()
	if !ok || data[0] != 2 {
		t.Errorf("expected [2], got %v (ok=%v)", data, ok)
	}
}

func TestSharedBufferReadResultReportsRetainedPacketOverwrite(t *testing.T) {
	sb := NewSharedBuffer(2)
	reader := sb.NewReader()

	for _, packet := range [][]byte{{0}, {1}, {2}, {3}} {
		sb.Write(packet)
	}

	result := reader.ReadResult()
	if !result.OK || !bytes.Equal(result.Data, []byte{2}) || result.Overwritten != 2 {
		t.Fatalf("ReadResult = %+v, want data [2], OK, and 2 overwritten packets", result)
	}
	result = reader.ReadResult()
	if !result.OK || !bytes.Equal(result.Data, []byte{3}) || result.Overwritten != 0 {
		t.Fatalf("next ReadResult = %+v, want data [3], OK, and no overwrite", result)
	}
}

func TestSharedBufferTryReadResultReportsRetainedPacketOverwrite(t *testing.T) {
	sb := NewSharedBuffer(2)
	reader := sb.NewReader()

	for _, packet := range [][]byte{{0}, {1}, {2}, {3}, {4}} {
		sb.Write(packet)
	}

	result := reader.TryReadResult()
	if !result.OK || !bytes.Equal(result.Data, []byte{3}) || result.Overwritten != 3 {
		t.Fatalf("TryReadResult = %+v, want data [3], OK, and 3 overwritten packets", result)
	}
	result = reader.TryReadResult()
	if !result.OK || !bytes.Equal(result.Data, []byte{4}) || result.Overwritten != 0 {
		t.Fatalf("next TryReadResult = %+v, want data [4], OK, and no overwrite", result)
	}
}

func TestSharedBufferLegacyReadWrappersRetainBehavior(t *testing.T) {
	sb := NewSharedBuffer(2)
	reader := sb.NewReader()

	for _, packet := range [][]byte{{0}, {1}, {2}, {3}} {
		sb.Write(packet)
	}

	data, ok := reader.Read()
	if !ok || !bytes.Equal(data, []byte{2}) {
		t.Fatalf("Read = (%v, %v), want ([2], true)", data, ok)
	}
	data, ok = reader.TryRead()
	if !ok || !bytes.Equal(data, []byte{3}) {
		t.Fatalf("TryRead = (%v, %v), want ([3], true)", data, ok)
	}
}

func TestSharedBufferTryRead(t *testing.T) {
	sb := NewSharedBuffer(64)
	r := sb.NewReader()

	// TryRead on empty buffer should return false (non-blocking)
	_, ok := r.TryRead()
	if ok {
		t.Error("TryRead should return false on empty buffer")
	}

	// Write data, then TryRead should succeed
	sb.Write([]byte{42})
	data, ok := r.TryRead()
	if !ok || !bytes.Equal(data, []byte{42}) {
		t.Errorf("expected [42], got %v (ok=%v)", data, ok)
	}

	// TryRead again should return false (no more data)
	_, ok = r.TryRead()
	if ok {
		t.Error("TryRead should return false when caught up")
	}
}

func TestSharedBufferClose(t *testing.T) {
	sb := NewSharedBuffer(64)
	sb.Write([]byte{1})
	r := sb.NewReader()

	data, ok := r.Read()
	if !ok || data[0] != 1 {
		t.Fatalf("expected [1], got %v", data)
	}

	done := make(chan struct{})
	go func() {
		_, ok := r.Read()
		if ok {
			t.Error("expected false after Close")
		}
		close(done)
	}()

	time.Sleep(10 * time.Millisecond)
	sb.Close()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Read did not unblock after Close")
	}
}
