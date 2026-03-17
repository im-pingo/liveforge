package av1

import "fmt"

// OBU types
const (
	OBUSequenceHeader    = 1
	OBUTemporalDelimiter = 2
	OBUFrameHeader       = 3
	OBUFrame             = 6
)

// ParseOBUHeader parses the first byte of an OBU header.
func ParseOBUHeader(data []byte) (obuType int, hasSize bool, err error) {
	if len(data) < 1 {
		return 0, false, fmt.Errorf("OBU header too short")
	}
	forbidden := (data[0] >> 7) & 0x01
	if forbidden != 0 {
		return 0, false, fmt.Errorf("OBU forbidden bit set")
	}
	obuType = int((data[0] >> 3) & 0x0F)
	hasSize = (data[0]>>1)&0x01 == 1
	return obuType, hasSize, nil
}

// BuildAV1CodecConfigRecord builds an AV1CodecConfigurationRecord for FMP4.
func BuildAV1CodecConfigRecord(seqHeaderOBU []byte) []byte {
	record := make([]byte, 4+len(seqHeaderOBU))
	record[0] = 0x81 // marker=1, version=1
	record[1] = 0x00 // seq_profile=0, seq_level_idx_0=0
	record[2] = 0x00 // seq_tier_0=0, high_bitdepth=0, twelve_bit=0, monochrome=0, chroma_subsampling_x=0, chroma_subsampling_y=0
	record[3] = 0x00 // chroma_sample_position=0, reserved
	copy(record[4:], seqHeaderOBU)
	return record
}
