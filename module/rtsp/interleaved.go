package rtsp

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
)

// WriteInterleaved writes an RTP/RTCP packet using RTSP TCP interleaved framing.
// Format: '$' + channel(1 byte) + length(2 bytes big-endian) + data
func WriteInterleaved(w io.Writer, channel uint8, data []byte) error {
	header := [4]byte{'$', channel, 0, 0}
	binary.BigEndian.PutUint16(header[2:], uint16(len(data)))
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	_, err := w.Write(data)
	return err
}

// ReadInterleaved reads a TCP interleaved frame.
// Returns the channel number and payload data.
// Caller must ensure the '$' byte has already been peeked/confirmed.
func ReadInterleaved(r *bufio.Reader) (channel uint8, data []byte, err error) {
	// Read '$' marker
	marker, err := r.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	if marker != '$' {
		return 0, nil, fmt.Errorf("expected '$' marker, got 0x%02x", marker)
	}
	// Read channel
	channel, err = r.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	// Read 2-byte length
	var lenBuf [2]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return 0, nil, err
	}
	length := binary.BigEndian.Uint16(lenBuf[:])
	// Read payload
	data = make([]byte, length)
	if _, err := io.ReadFull(r, data); err != nil {
		return 0, nil, err
	}
	return channel, data, nil
}
