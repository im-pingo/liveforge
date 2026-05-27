package pool

import (
	"sync"
)

const (
	smallBufSize  = 256
	mediumBufSize = 4096
	largeBufSize  = 65536
)

var (
	smallPool = sync.Pool{
		New: func() any { b := make([]byte, 0, smallBufSize); return &b },
	}
	mediumPool = sync.Pool{
		New: func() any { b := make([]byte, 0, mediumBufSize); return &b },
	}
	largePool = sync.Pool{
		New: func() any { b := make([]byte, 0, largeBufSize); return &b },
	}
)

// GetBuffer returns a pooled byte slice with at least the given capacity.
// The returned slice has length 0 and the caller must append to it.
// Call PutBuffer when done to return it to the pool.
func GetBuffer(minCap int) *[]byte {
	if minCap <= smallBufSize {
		return smallPool.Get().(*[]byte)
	}
	if minCap <= mediumBufSize {
		return mediumPool.Get().(*[]byte)
	}
	if minCap <= largeBufSize {
		return largePool.Get().(*[]byte)
	}
	b := make([]byte, 0, minCap)
	return &b
}

// PutBuffer returns a buffer to the pool. The buffer is reset to zero length.
// Nil pointers and oversized buffers (>128KB) are silently discarded.
func PutBuffer(b *[]byte) {
	if b == nil {
		return
	}
	c := cap(*b)
	*b = (*b)[:0]

	if c > largeBufSize*2 {
		return
	}
	if c >= largeBufSize {
		largePool.Put(b)
	} else if c >= mediumBufSize {
		mediumPool.Put(b)
	} else {
		smallPool.Put(b)
	}
}
