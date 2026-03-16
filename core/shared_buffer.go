package core

import (
	"github.com/im-pingo/liveforge/pkg/util"
)

// SharedBuffer distributes muxed output packets to multiple readers.
// It wraps a RingBuffer[[]byte] for efficient single-writer, multi-reader distribution.
type SharedBuffer struct {
	rb *util.RingBuffer[[]byte]
}

// NewSharedBuffer creates a new shared buffer with the given capacity.
func NewSharedBuffer(size int) *SharedBuffer {
	return &SharedBuffer{
		rb: util.NewRingBuffer[[]byte](size),
	}
}

// Write adds a muxed packet to the shared buffer.
func (sb *SharedBuffer) Write(packet []byte) {
	sb.rb.Write(packet)
}

// NewReader creates a new reader starting at the oldest available position.
func (sb *SharedBuffer) NewReader() *SharedBufferReader {
	return &SharedBufferReader{
		reader: sb.rb.NewReader(),
	}
}

// SharedBufferReader is a per-subscriber cursor into a SharedBuffer.
type SharedBufferReader struct {
	reader *util.RingReader[[]byte]
}

// Read returns the next packet, blocking until data is available.
func (r *SharedBufferReader) Read() ([]byte, bool) {
	return r.reader.Read()
}

// TryRead attempts a non-blocking read.
func (r *SharedBufferReader) TryRead() ([]byte, bool) {
	return r.reader.TryRead()
}
