package ps

import (
	"encoding/binary"
	"fmt"

	"github.com/im-pingo/liveforge/pkg/avframe"
)

// Muxer generates MPEG-PS packs from AVFrames.
type Muxer struct {
	muxRate uint32 // bytes/sec, used in pack header
}

// NewMuxer creates a new PS muxer.
func NewMuxer() *Muxer {
	return &Muxer{muxRate: 50000} // ~400 kbps default
}

// Pack wraps a single AVFrame into a complete PS pack (pack header + optional system header + PES).
// Returns the raw PS bytes.
func (m *Muxer) Pack(frame *avframe.AVFrame) ([]byte, error) {
	if frame == nil || len(frame.Payload) == 0 {
		return nil, fmt.Errorf("ps: empty frame")
	}

	isKeyframe := frame.FrameType == avframe.FrameTypeKeyframe ||
		frame.FrameType == avframe.FrameTypeSequenceHeader

	scr := msToSCR(frame.DTS)

	var buf []byte

	// Pack header (14 bytes)
	buf = append(buf, m.buildPackHeader(scr)...)

	// System header only on keyframes
	if isKeyframe && frame.MediaType == avframe.MediaTypeVideo {
		buf = append(buf, m.buildSystemHeader()...)
	}

	// PES packet
	streamID := byte(PESVideoStreamID)
	if frame.MediaType == avframe.MediaTypeAudio {
		streamID = byte(PESAudioStreamID)
	}

	pes := buildPES(streamID, frame.Payload, frame.DTS, frame.PTS)
	buf = append(buf, pes...)

	return buf, nil
}

// buildPackHeader creates a 14-byte MPEG-2 PS pack header.
func (m *Muxer) buildPackHeader(scr int64) []byte {
	buf := make([]byte, 14)
	// Start code
	binary.BigEndian.PutUint32(buf[0:4], PackHeaderStartCode)

	// SCR fields (MPEG-2 format): 6 bytes
	scrBase := scr / 300
	scrExt := scr % 300

	// Byte 4: '01' marker(2) + SCR[32..30](3) + marker(1) + SCR[29..28](2)
	buf[4] = 0x44 | // '01' + marker bit at position 2
		byte((scrBase>>27)&0x38) | // SCR[32..30]
		byte((scrBase>>28)&0x03) // SCR[29..28]
	// Byte 5-6: SCR[27..13](15) + marker(1)
	buf[5] = byte(scrBase >> 20)
	buf[6] = byte(scrBase>>12)&0xF8 | 0x04 | byte((scrBase>>10)&0x03)
	// Byte 7-8: SCR[12..0](13) + marker(1) + SCR_ext[8..7](2)
	buf[7] = byte(scrBase >> 5)
	buf[8] = byte(scrBase<<3)&0xF8 | 0x04 | byte((scrExt>>7)&0x03)
	// Byte 9: SCR_ext[6..0](7) + marker(1)
	buf[9] = byte(scrExt<<1) | 0x01

	// Mux rate (22 bits, in 50 byte/s units): bytes 10-12
	muxRate50 := m.muxRate / 50
	buf[10] = byte(muxRate50>>14) & 0xFF
	buf[11] = byte(muxRate50 >> 6)
	buf[12] = byte(muxRate50<<2) | 0x03 // marker bits

	// Stuffing length = 0
	buf[13] = 0xF8 // reserved(5) + pack_stuffing_length(3) = 0

	return buf
}

// buildSystemHeader creates a system header.
func (m *Muxer) buildSystemHeader() []byte {
	// Minimal system header: 6 (start + length) + 6 (fixed) + 3*2 (streams) = 18 bytes
	headerLen := 6 + 3 + 3 // fixed + video stream + audio stream
	buf := make([]byte, 6+headerLen)

	binary.BigEndian.PutUint32(buf[0:4], SystemHeaderStartCode)
	binary.BigEndian.PutUint16(buf[4:6], uint16(headerLen))

	// Rate bound (22 bits)
	rateBound := m.muxRate / 50
	buf[6] = 0x80 | byte(rateBound>>15)&0x7F
	buf[7] = byte(rateBound >> 7)
	buf[8] = byte(rateBound<<1) | 0x01

	// Audio bound(6) + fixed_flag(1) + CSPS_flag(1)
	buf[9] = 0x04 | 0x01 // 1 audio stream, CSPS=1
	// System audio lock + system video lock + reserved + video bound
	buf[10] = 0xE0 | 0x01 // locked, 1 video stream
	// Packet rate restriction
	buf[11] = 0x7F // unrestricted

	// Video stream entry
	buf[12] = PESVideoStreamID
	buf[13] = 0xE0 | 0x08 // P-STD_buffer_bound_scale=1 (1024 units)
	buf[14] = 0x80         // buffer size 128KB

	// Audio stream entry
	buf[15] = PESAudioStreamID
	buf[16] = 0xC0 | 0x04 // P-STD_buffer_bound_scale=0 (128 units)
	buf[17] = 0x20         // buffer size 4KB

	return buf
}

// buildPES creates a PES packet with optional PTS/DTS.
func buildPES(streamID byte, payload []byte, dtsMs, ptsMs int64) []byte {
	dts := msToPESTimestamp(dtsMs)
	pts := msToPESTimestamp(ptsMs)

	hasPTS := true
	hasDTS := dts != pts

	headerDataLen := 5 // PTS only
	ptsDtsFlags := byte(0x80)
	if hasDTS {
		headerDataLen = 10
		ptsDtsFlags = 0xC0
	}

	pesLen := 3 + headerDataLen + len(payload)
	if pesLen > 0xFFFF {
		pesLen = 0 // unbounded for large video frames
	}

	buf := make([]byte, 0, 6+3+headerDataLen+len(payload))

	// Start code prefix + stream_id
	buf = append(buf, 0x00, 0x00, 0x01, streamID)
	// PES packet length (16 bits)
	buf = append(buf, byte(pesLen>>8), byte(pesLen))

	// Optional PES header
	buf = append(buf, 0x80)         // flags: MPEG-2
	buf = append(buf, ptsDtsFlags)  // PTS/DTS flags
	buf = append(buf, byte(headerDataLen))

	if hasPTS {
		buf = append(buf, encodePESTimestamp(pts, ptsDtsFlags>>4)...)
	}
	if hasDTS {
		buf = append(buf, encodePESTimestamp(dts, 0x01)...)
	}

	buf = append(buf, payload...)
	return buf
}

// encodePESTimestamp encodes a 33-bit timestamp into 5 PES bytes.
func encodePESTimestamp(ts int64, marker byte) []byte {
	buf := make([]byte, 5)
	buf[0] = (marker << 4) | byte((ts>>29)&0x0E) | 0x01
	buf[1] = byte(ts >> 22)
	buf[2] = byte((ts>>14)&0xFE) | 0x01
	buf[3] = byte(ts >> 7)
	buf[4] = byte((ts<<1)&0xFE) | 0x01
	return buf
}

// msToSCR converts milliseconds to 27MHz SCR (System Clock Reference).
func msToSCR(ms int64) int64 {
	return ms * 27000
}

// msToPESTimestamp converts milliseconds to 90kHz PES timestamp.
func msToPESTimestamp(ms int64) int64 {
	return ms * 90
}
