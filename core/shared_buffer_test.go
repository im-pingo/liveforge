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
