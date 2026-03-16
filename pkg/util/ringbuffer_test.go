package util

import (
	"testing"
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
