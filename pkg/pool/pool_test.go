package pool

import (
	"testing"
)

func TestGetPutBuffer(t *testing.T) {
	tests := []struct {
		name   string
		minCap int
		wantMinCap int
	}{
		{"small", 100, smallBufSize},
		{"medium", 1000, mediumBufSize},
		{"large", 10000, largeBufSize},
		{"oversized", 200000, 200000},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := GetBuffer(tt.minCap)
			if b == nil {
				t.Fatal("GetBuffer returned nil")
			}
			if len(*b) != 0 {
				t.Errorf("expected length 0, got %d", len(*b))
			}
			if cap(*b) < tt.wantMinCap {
				t.Errorf("expected cap >= %d, got %d", tt.wantMinCap, cap(*b))
			}
			*b = append(*b, 1, 2, 3)
			PutBuffer(b)
		})
	}
}

func TestPutNilBuffer(t *testing.T) {
	PutBuffer(nil)
}

func TestBufferReuse(t *testing.T) {
	b1 := GetBuffer(100)
	ptr1 := &(*b1)[0:cap(*b1)][0]
	PutBuffer(b1)

	b2 := GetBuffer(100)
	ptr2 := &(*b2)[0:cap(*b2)][0]

	if ptr1 != ptr2 {
		t.Log("buffer was not reused (may happen under contention)")
	}
	PutBuffer(b2)
}

func BenchmarkGetPutSmall(b *testing.B) {
	for b.Loop() {
		buf := GetBuffer(100)
		*buf = append(*buf, make([]byte, 100)...)
		PutBuffer(buf)
	}
}

func BenchmarkGetPutMedium(b *testing.B) {
	for b.Loop() {
		buf := GetBuffer(2000)
		*buf = append(*buf, make([]byte, 2000)...)
		PutBuffer(buf)
	}
}

func BenchmarkAllocSmall(b *testing.B) {
	for b.Loop() {
		buf := make([]byte, 0, 256)
		buf = append(buf, make([]byte, 100)...)
		_ = buf
	}
}

func BenchmarkAllocMedium(b *testing.B) {
	for b.Loop() {
		buf := make([]byte, 0, 4096)
		buf = append(buf, make([]byte, 2000)...)
		_ = buf
	}
}
