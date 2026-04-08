package rtmp

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Chunk format types.
const (
	chunkFmt0 = 0 // 11-byte message header (full)
	chunkFmt1 = 1 // 7-byte header (delta timestamp, length, type)
	chunkFmt2 = 2 // 3-byte header (delta timestamp only)
	chunkFmt3 = 3 // 0-byte header (continuation)
)

// chunkHeader stores per-CSID state for delta encoding/decoding.
type chunkHeader struct {
	timestamp uint32
	length    uint32
	typeID    uint8
	streamID  uint32
}

// ChunkReader reads RTMP chunks from a stream and reassembles messages.
type ChunkReader struct {
	r         io.Reader
	chunkSize int
	headers   map[uint32]*chunkHeader
	buffers   map[uint32][]byte // partial message data per CSID
}

// NewChunkReader creates a new chunk reader.
func NewChunkReader(r io.Reader, chunkSize int) *ChunkReader {
	return &ChunkReader{
		r:         r,
		chunkSize: chunkSize,
		headers:   make(map[uint32]*chunkHeader),
		buffers:   make(map[uint32][]byte),
	}
}

// SetChunkSize updates the chunk size for reading.
func (cr *ChunkReader) SetChunkSize(size int) {
	cr.chunkSize = size
}

// ReadMessage reads chunks until a complete message is assembled.
func (cr *ChunkReader) ReadMessage() (*Message, error) {
	for {
		csid, chunkFmt, err := cr.readBasicHeader()
		if err != nil {
			return nil, err
		}

		prev := cr.headers[csid]
		if prev == nil {
			prev = &chunkHeader{}
			cr.headers[csid] = prev
		}

		if err := cr.readMessageHeader(chunkFmt, prev); err != nil {
			return nil, err
		}

		// Read chunk data
		remaining := int(prev.length) - len(cr.buffers[csid])
		toRead := remaining
		if toRead > cr.chunkSize {
			toRead = cr.chunkSize
		}

		data := make([]byte, toRead)
		if _, err := io.ReadFull(cr.r, data); err != nil {
			return nil, fmt.Errorf("read chunk data: %w", err)
		}
		cr.buffers[csid] = append(cr.buffers[csid], data...)

		// Check if message is complete
		if len(cr.buffers[csid]) >= int(prev.length) {
			payload := cr.buffers[csid][:prev.length]
			cr.buffers[csid] = nil

			return &Message{
				TypeID:    prev.typeID,
				Length:    prev.length,
				Timestamp: prev.timestamp,
				StreamID:  prev.streamID,
				Payload:   payload,
			}, nil
		}
	}
}

func (cr *ChunkReader) readBasicHeader() (csid uint32, chunkFormat uint8, err error) {
	var b [1]byte
	if _, err = io.ReadFull(cr.r, b[:]); err != nil {
		return 0, 0, err
	}

	chunkFormat = (b[0] >> 6) & 0x03
	csid = uint32(b[0] & 0x3F)

	if csid == 0 {
		// 2-byte form
		var b2 [1]byte
		if _, err = io.ReadFull(cr.r, b2[:]); err != nil {
			return
		}
		csid = uint32(b2[0]) + 64
	} else if csid == 1 {
		// 3-byte form
		var b3 [2]byte
		if _, err = io.ReadFull(cr.r, b3[:]); err != nil {
			return
		}
		csid = uint32(b3[1])*256 + uint32(b3[0]) + 64
	}

	return csid, chunkFormat, nil
}

func (cr *ChunkReader) readMessageHeader(chunkFmt uint8, h *chunkHeader) error {
	switch chunkFmt {
	case chunkFmt0:
		// 11 bytes: timestamp(3) + length(3) + type(1) + streamID(4)
		var buf [11]byte
		if _, err := io.ReadFull(cr.r, buf[:]); err != nil {
			return err
		}
		h.timestamp = uint32(buf[0])<<16 | uint32(buf[1])<<8 | uint32(buf[2])
		h.length = uint32(buf[3])<<16 | uint32(buf[4])<<8 | uint32(buf[5])
		h.typeID = buf[6]
		h.streamID = binary.LittleEndian.Uint32(buf[7:11])

		if h.timestamp == 0xFFFFFF {
			var ext [4]byte
			if _, err := io.ReadFull(cr.r, ext[:]); err != nil {
				return err
			}
			h.timestamp = binary.BigEndian.Uint32(ext[:])
		}

	case chunkFmt1:
		// 7 bytes: timestamp delta(3) + length(3) + type(1)
		var buf [7]byte
		if _, err := io.ReadFull(cr.r, buf[:]); err != nil {
			return err
		}
		delta := uint32(buf[0])<<16 | uint32(buf[1])<<8 | uint32(buf[2])
		h.length = uint32(buf[3])<<16 | uint32(buf[4])<<8 | uint32(buf[5])
		h.typeID = buf[6]

		if delta == 0xFFFFFF {
			var ext [4]byte
			if _, err := io.ReadFull(cr.r, ext[:]); err != nil {
				return err
			}
			delta = binary.BigEndian.Uint32(ext[:])
		}
		h.timestamp += delta

	case chunkFmt2:
		// 3 bytes: timestamp delta(3)
		var buf [3]byte
		if _, err := io.ReadFull(cr.r, buf[:]); err != nil {
			return err
		}
		delta := uint32(buf[0])<<16 | uint32(buf[1])<<8 | uint32(buf[2])

		if delta == 0xFFFFFF {
			var ext [4]byte
			if _, err := io.ReadFull(cr.r, ext[:]); err != nil {
				return err
			}
			delta = binary.BigEndian.Uint32(ext[:])
		}
		h.timestamp += delta

	case chunkFmt3:
		// No message header
	}

	return nil
}

// ChunkWriter writes RTMP messages as chunks.
type ChunkWriter struct {
	w         io.Writer
	chunkSize int
	buf       []byte // reusable write buffer to batch syscalls
}

// NewChunkWriter creates a new chunk writer.
func NewChunkWriter(w io.Writer, chunkSize int) *ChunkWriter {
	return &ChunkWriter{
		w:         w,
		chunkSize: chunkSize,
		buf:       make([]byte, 0, chunkSize+16),
	}
}

// SetChunkSize updates the chunk size for writing.
func (cw *ChunkWriter) SetChunkSize(size int) {
	cw.chunkSize = size
}

// WriteMessage writes a complete message as one or more chunks.
// Always uses fmt 0 for the first chunk and fmt 3 for continuation chunks.
// All chunk headers and payload data are buffered and written in a single
// syscall to minimize write overhead.
func (cw *ChunkWriter) WriteMessage(csid uint32, msg *Message) error {
	// Estimate total size: headers + payload + continuation headers.
	// Continuation header (1-3 bytes) every chunkSize bytes.
	nChunks := (len(msg.Payload) + cw.chunkSize - 1) / cw.chunkSize
	if nChunks == 0 {
		nChunks = 1
	}
	estimatedSize := 3 + 11 + 4 + len(msg.Payload) + nChunks*3
	cw.buf = cw.buf[:0]
	if cap(cw.buf) < estimatedSize {
		cw.buf = make([]byte, 0, estimatedSize)
	}

	// First chunk: fmt 0 with full header
	cw.buf = appendBasicHeader(cw.buf, chunkFmt0, csid)

	// Message header (11 bytes for fmt 0)
	ts := msg.Timestamp
	if ts >= 0xFFFFFF {
		cw.buf = append(cw.buf, 0xFF, 0xFF, 0xFF)
	} else {
		cw.buf = append(cw.buf, byte(ts>>16), byte(ts>>8), byte(ts))
	}
	cw.buf = append(cw.buf, byte(msg.Length>>16), byte(msg.Length>>8), byte(msg.Length))
	cw.buf = append(cw.buf, msg.TypeID)
	cw.buf = binary.LittleEndian.AppendUint32(cw.buf, msg.StreamID)

	if ts >= 0xFFFFFF {
		cw.buf = binary.BigEndian.AppendUint32(cw.buf, ts)
	}

	// Write first chunk data
	payload := msg.Payload
	firstChunkSize := cw.chunkSize
	if firstChunkSize > len(payload) {
		firstChunkSize = len(payload)
	}
	cw.buf = append(cw.buf, payload[:firstChunkSize]...)

	// Write continuation chunks (fmt 3)
	offset := firstChunkSize
	for offset < len(payload) {
		cw.buf = appendBasicHeader(cw.buf, chunkFmt3, csid)

		chunkLen := cw.chunkSize
		if offset+chunkLen > len(payload) {
			chunkLen = len(payload) - offset
		}
		cw.buf = append(cw.buf, payload[offset:offset+chunkLen]...)
		offset += chunkLen
	}

	_, err := cw.w.Write(cw.buf)
	return err
}

func appendBasicHeader(dst []byte, fmt uint8, csid uint32) []byte {
	if csid >= 2 && csid <= 63 {
		return append(dst, (fmt<<6)|byte(csid))
	} else if csid >= 64 && csid <= 319 {
		return append(dst, fmt<<6, byte(csid-64))
	}
	id := csid - 64
	return append(dst, (fmt<<6)|1, byte(id), byte(id>>8))
}
