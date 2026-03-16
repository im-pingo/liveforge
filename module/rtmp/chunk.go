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
}

// NewChunkWriter creates a new chunk writer.
func NewChunkWriter(w io.Writer, chunkSize int) *ChunkWriter {
	return &ChunkWriter{
		w:         w,
		chunkSize: chunkSize,
	}
}

// SetChunkSize updates the chunk size for writing.
func (cw *ChunkWriter) SetChunkSize(size int) {
	cw.chunkSize = size
}

// WriteMessage writes a complete message as one or more chunks.
// Always uses fmt 0 for the first chunk and fmt 3 for continuation chunks.
func (cw *ChunkWriter) WriteMessage(csid uint32, msg *Message) error {
	// First chunk: fmt 0 with full header
	basicHeader := cw.encodeBasicHeader(chunkFmt0, csid)
	if _, err := cw.w.Write(basicHeader); err != nil {
		return err
	}

	// Message header (11 bytes for fmt 0)
	var header [11]byte
	ts := msg.Timestamp
	if ts >= 0xFFFFFF {
		header[0] = 0xFF
		header[1] = 0xFF
		header[2] = 0xFF
	} else {
		header[0] = byte(ts >> 16)
		header[1] = byte(ts >> 8)
		header[2] = byte(ts)
	}
	header[3] = byte(msg.Length >> 16)
	header[4] = byte(msg.Length >> 8)
	header[5] = byte(msg.Length)
	header[6] = msg.TypeID
	binary.LittleEndian.PutUint32(header[7:11], msg.StreamID)

	if _, err := cw.w.Write(header[:]); err != nil {
		return err
	}

	if ts >= 0xFFFFFF {
		var ext [4]byte
		binary.BigEndian.PutUint32(ext[:], ts)
		if _, err := cw.w.Write(ext[:]); err != nil {
			return err
		}
	}

	// Write first chunk data
	payload := msg.Payload
	firstChunkSize := cw.chunkSize
	if firstChunkSize > len(payload) {
		firstChunkSize = len(payload)
	}
	if _, err := cw.w.Write(payload[:firstChunkSize]); err != nil {
		return err
	}

	// Write continuation chunks (fmt 3)
	offset := firstChunkSize
	for offset < len(payload) {
		contHeader := cw.encodeBasicHeader(chunkFmt3, csid)
		if _, err := cw.w.Write(contHeader); err != nil {
			return err
		}

		chunkLen := cw.chunkSize
		if offset+chunkLen > len(payload) {
			chunkLen = len(payload) - offset
		}
		if _, err := cw.w.Write(payload[offset : offset+chunkLen]); err != nil {
			return err
		}
		offset += chunkLen
	}

	return nil
}

func (cw *ChunkWriter) encodeBasicHeader(fmt uint8, csid uint32) []byte {
	if csid >= 2 && csid <= 63 {
		return []byte{(fmt << 6) | byte(csid)}
	} else if csid >= 64 && csid <= 319 {
		return []byte{fmt << 6, byte(csid - 64)}
	} else {
		id := csid - 64
		return []byte{(fmt << 6) | 1, byte(id), byte(id >> 8)}
	}
}
