package ts

import "encoding/binary"

// BuildPESHeader builds a PES header with optional PTS and DTS.
// streamID: 0xE0 for video, 0xC0 for audio.
// If dts == pts, only PTS is written.
func BuildPESHeader(streamID byte, pts, dts int64, payloadLen int) []byte {
	hasDTS := dts != pts

	headerDataLen := 5 // PTS only
	if hasDTS {
		headerDataLen = 10 // PTS + DTS
	}

	// PES header: 3 (start code) + 1 (stream_id) + 2 (PES_packet_length) + 3 (flags + header_data_length) + headerDataLen
	header := make([]byte, 9+headerDataLen)

	// Start code prefix
	header[0] = 0x00
	header[1] = 0x00
	header[2] = 0x01
	header[3] = streamID

	// PES packet length: 0 for unbounded video, or actual length for audio
	pesLen := 0
	if streamID >= 0xC0 && streamID < 0xE0 {
		// Audio: set actual length (3 bytes flags + headerDataLen + payload)
		pesLen = 3 + headerDataLen + payloadLen
		if pesLen > 0xFFFF {
			pesLen = 0
		}
	}
	binary.BigEndian.PutUint16(header[4:6], uint16(pesLen))

	// Flags
	header[6] = 0x80 // '10' marker bits, no scrambling, no priority, no alignment, no copyright, no original
	if hasDTS {
		header[7] = 0xC0 // PTS_DTS_flags = 11
	} else {
		header[7] = 0x80 // PTS_DTS_flags = 10
	}
	header[8] = byte(headerDataLen)

	// Write PTS
	writePTS(header[9:], pts, hasDTS)

	// Write DTS
	if hasDTS {
		writeDTS(header[14:], dts)
	}

	return header
}

// writePTS encodes a 33-bit PTS into 5 bytes.
// If hasDTS is true, the marker nibble is 0x3 (PTS with DTS), otherwise 0x2 (PTS only).
func writePTS(buf []byte, ts int64, hasDTS bool) {
	// Convert ms to 90kHz
	ts90 := ts * 90

	marker := byte(0x21) // '0010 xxx1' for PTS only
	if hasDTS {
		marker = byte(0x31) // '0011 xxx1' for PTS+DTS
	}

	buf[0] = marker | byte((ts90>>29)&0x0E)
	buf[1] = byte(ts90 >> 22)
	buf[2] = byte((ts90>>14)&0xFE) | 0x01
	buf[3] = byte(ts90 >> 7)
	buf[4] = byte((ts90<<1)&0xFE) | 0x01
}

// writeDTS encodes a 33-bit DTS into 5 bytes.
func writeDTS(buf []byte, ts int64) {
	ts90 := ts * 90

	buf[0] = 0x11 | byte((ts90>>29)&0x0E) // '0001 xxx1'
	buf[1] = byte(ts90 >> 22)
	buf[2] = byte((ts90>>14)&0xFE) | 0x01
	buf[3] = byte(ts90 >> 7)
	buf[4] = byte((ts90<<1)&0xFE) | 0x01
}

// PacketizePES splits a PES (header + payload) into 188-byte TS packets.
// The first packet has payload_unit_start_indicator=1.
// continuityCounter is incremented for each packet and the updated value is returned.
func PacketizePES(pid uint16, pesData []byte, continuityCounter *uint8, isKeyframe bool) []byte {
	var result []byte
	offset := 0
	first := true

	for offset < len(pesData) {
		pkt := make([]byte, PacketSize)
		pkt[0] = SyncByte

		// PID
		pkt[1] = byte((pid >> 8) & 0x1F)
		pkt[2] = byte(pid & 0xFF)

		if first {
			pkt[1] |= 0x40 // payload_unit_start_indicator
			first = false
		}

		headerLen := 4
		remaining := len(pesData) - offset
		maxPayload := PacketSize - headerLen

		if remaining < maxPayload {
			// Need adaptation field for stuffing
			stuffingLen := maxPayload - remaining
			if stuffingLen == 1 {
				// Adaptation field with length=0
				pkt[3] = 0x30 | (*continuityCounter & 0x0F) // adaptation + payload
				pkt[4] = 0x00                                // adaptation_field_length = 0
				headerLen = 5
			} else {
				pkt[3] = 0x30 | (*continuityCounter & 0x0F) // adaptation + payload
				adaptLen := stuffingLen - 1                  // -1 for adaptation_field_length byte
				pkt[4] = byte(adaptLen)
				if adaptLen > 0 {
					pkt[5] = 0x00 // flags (no PCR etc.)
					// Fill remaining adaptation bytes with 0xFF
					for i := 6; i < 5+int(adaptLen); i++ {
						pkt[i] = 0xFF
					}
				}
				headerLen = 4 + 1 + int(adaptLen) // 4 TS + 1 adapt_len + adapt body
			}
		} else {
			pkt[3] = 0x10 | (*continuityCounter & 0x0F) // payload only
		}

		payloadStart := headerLen
		payloadLen := PacketSize - payloadStart
		if payloadLen > remaining {
			payloadLen = remaining
		}

		copy(pkt[payloadStart:], pesData[offset:offset+payloadLen])
		offset += payloadLen

		*continuityCounter = (*continuityCounter + 1) & 0x0F
		result = append(result, pkt...)
	}

	return result
}
