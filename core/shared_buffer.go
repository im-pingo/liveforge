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

// Close signals all readers to unblock and return (nil, false).
func (sb *SharedBuffer) Close() {
	sb.rb.Close()
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

// SharedBufferReadResult binds overwrite metadata to the packet returned by
// one SharedBuffer read.
type SharedBufferReadResult struct {
	Data        []byte
	OK          bool
	Overwritten int64
}

// Read returns the next packet, blocking until data is available.
func (r *SharedBufferReader) Read() ([]byte, bool) {
	result := r.ReadResult()
	return result.Data, result.OK
}

// ReadResult returns the next packet and its overwrite metadata, blocking
// until data is available or the buffer or reader is closed.
func (r *SharedBufferReader) ReadResult() SharedBufferReadResult {
	result := r.reader.ReadResult()
	return SharedBufferReadResult{
		Data:        result.Value,
		OK:          result.OK,
		Overwritten: result.Overwritten,
	}
}

// Close marks this reader as closed, unblocking any in-progress Read().
func (r *SharedBufferReader) Close() {
	r.reader.Close()
}

// TryRead attempts a non-blocking read.
func (r *SharedBufferReader) TryRead() ([]byte, bool) {
	result := r.TryReadResult()
	return result.Data, result.OK
}

// TryReadResult attempts a non-blocking read and returns overwrite metadata
// from the same operation as the packet.
func (r *SharedBufferReader) TryReadResult() SharedBufferReadResult {
	result := r.reader.TryReadResult()
	return SharedBufferReadResult{
		Data:        result.Value,
		OK:          result.OK,
		Overwritten: result.Overwritten,
	}
}
