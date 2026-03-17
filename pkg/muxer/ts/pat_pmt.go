package ts

import "encoding/binary"

// BuildPAT generates a single 188-byte TS packet containing a PAT.
func BuildPAT(continuityCounter uint8) []byte {
	pkt := make([]byte, PacketSize)

	// TS header (4 bytes)
	pkt[0] = SyncByte
	pkt[1] = 0x40 // payload_unit_start_indicator=1, PID high=0
	pkt[2] = 0x00 // PID low=0 (PAT)
	pkt[3] = 0x10 | (continuityCounter & 0x0F) // adaptation=00, payload=01

	// Pointer field
	pkt[4] = 0x00

	// PAT table
	tableStart := 5
	pat := pkt[tableStart:]
	pat[0] = 0x00 // table_id = 0 (PAT)
	// section_syntax_indicator=1, '0', reserved=11, section_length (12 bits)
	// PAT payload: 5 bytes (transport_stream_id, version, section_number, last_section_number) + 4 bytes (program) + 4 bytes CRC = 13
	sectionLen := 13
	pat[1] = 0xB0 | byte((sectionLen>>8)&0x0F)
	pat[2] = byte(sectionLen & 0xFF)
	pat[3] = 0x00 // transport_stream_id high
	pat[4] = 0x01 // transport_stream_id low
	pat[5] = 0xC1 // reserved=11, version=0, current_next=1
	pat[6] = 0x00 // section_number
	pat[7] = 0x00 // last_section_number

	// Program entry: program_number=1, PMT PID=0x1000
	pat[8] = 0x00
	pat[9] = 0x01 // program_number = 1
	pat[10] = 0xE0 | byte((PIDPmt>>8)&0x1F)
	pat[11] = byte(PIDPmt & 0xFF)

	// CRC32
	crc := CRC32MPEG2(pkt[tableStart : tableStart+12])
	binary.BigEndian.PutUint32(pkt[tableStart+12:], crc)

	// Fill rest with 0xFF (stuffing)
	for i := tableStart + 16; i < PacketSize; i++ {
		pkt[i] = 0xFF
	}

	return pkt
}

// BuildPMT generates a single 188-byte TS packet containing a PMT.
func BuildPMT(videoCodec, audioCodec byte, continuityCounter uint8) []byte {
	pkt := make([]byte, PacketSize)

	// TS header
	pkt[0] = SyncByte
	pkt[1] = 0x40 | byte((PIDPmt>>8)&0x1F)
	pkt[2] = byte(PIDPmt & 0xFF)
	pkt[3] = 0x10 | (continuityCounter & 0x0F)

	// Pointer field
	pkt[4] = 0x00

	tableStart := 5

	// Build PMT section body first to calculate section length
	var body []byte

	// Program info: PCR PID, program_info_length=0
	body = append(body, 0xE0|byte((PIDPCR>>8)&0x1F), byte(PIDPCR&0xFF))
	body = append(body, 0xF0, 0x00) // reserved + program_info_length = 0

	// Video stream entry
	if videoCodec != 0 {
		body = append(body, videoCodec) // stream_type
		body = append(body, 0xE0|byte((PIDVideo>>8)&0x1F), byte(PIDVideo&0xFF))
		body = append(body, 0xF0, 0x00) // reserved + ES_info_length = 0
	}

	// Audio stream entry
	if audioCodec != 0 {
		body = append(body, audioCodec) // stream_type
		body = append(body, 0xE0|byte((PIDAudio>>8)&0x1F), byte(PIDAudio&0xFF))
		body = append(body, 0xF0, 0x00) // reserved + ES_info_length = 0
	}

	// section_length = 5 (header after length field) + body + 4 (CRC)
	sectionLen := 5 + len(body) + 4

	pmt := pkt[tableStart:]
	pmt[0] = 0x02 // table_id = 2 (PMT)
	pmt[1] = 0xB0 | byte((sectionLen>>8)&0x0F)
	pmt[2] = byte(sectionLen & 0xFF)
	pmt[3] = 0x00 // program_number high
	pmt[4] = 0x01 // program_number low = 1
	pmt[5] = 0xC1 // reserved=11, version=0, current_next=1
	pmt[6] = 0x00 // section_number
	pmt[7] = 0x00 // last_section_number

	copy(pmt[8:], body)

	// CRC32 over the whole section (from table_id to just before CRC)
	crcOffset := 8 + len(body)
	crc := CRC32MPEG2(pkt[tableStart : tableStart+crcOffset])
	binary.BigEndian.PutUint32(pkt[tableStart+crcOffset:], crc)

	// Stuffing
	for i := tableStart + crcOffset + 4; i < PacketSize; i++ {
		pkt[i] = 0xFF
	}

	return pkt
}
