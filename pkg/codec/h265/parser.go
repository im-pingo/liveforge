package h265

import (
	"encoding/binary"
	"fmt"
)

// HEVC NAL unit types
const (
	NALTypeVPS    = 32
	NALTypeSPS    = 33
	NALTypePPS    = 34
	NALTypeIDRWLP = 19
	NALTypeIDRNLP = 20
)

// HVCCToAnnexB converts HVCC format (4-byte big-endian length-prefixed NALUs) to Annex-B (start code prefixed).
func HVCCToAnnexB(data []byte) []byte {
	var result []byte
	startCode := []byte{0x00, 0x00, 0x00, 0x01}
	offset := 0
	for offset+4 <= len(data) {
		naluLen := int(binary.BigEndian.Uint32(data[offset:]))
		offset += 4
		if naluLen <= 0 || offset+naluLen > len(data) {
			break
		}
		result = append(result, startCode...)
		result = append(result, data[offset:offset+naluLen]...)
		offset += naluLen
	}
	return result
}

// ExtractVPSSPSPPSFromHVCRecord extracts VPS, SPS, PPS NALUs from HEVCDecoderConfigurationRecord.
// Format per ISO 14496-15:
// - 22 bytes of fixed config
// - numOfArrays (1 byte)
// - For each array: array_completeness+reserved+NAL_unit_type (1), numNalus (2), then for each: naluLength (2) + naluData
func ExtractVPSSPSPPSFromHVCRecord(data []byte) (vps, sps, pps []byte, err error) {
	if len(data) < 23 {
		return nil, nil, nil, fmt.Errorf("HEVCDecoderConfigurationRecord too short: %d bytes", len(data))
	}
	numArrays := int(data[22])
	offset := 23
	for i := 0; i < numArrays; i++ {
		if offset >= len(data) {
			break
		}
		nalType := data[offset] & 0x3F
		offset++
		if offset+2 > len(data) {
			break
		}
		numNalus := int(binary.BigEndian.Uint16(data[offset:]))
		offset += 2
		for j := 0; j < numNalus; j++ {
			if offset+2 > len(data) {
				break
			}
			naluLen := int(binary.BigEndian.Uint16(data[offset:]))
			offset += 2
			if offset+naluLen > len(data) {
				break
			}
			naluData := data[offset : offset+naluLen]
			switch nalType {
			case NALTypeVPS:
				if vps == nil {
					vps = naluData
				}
			case NALTypeSPS:
				if sps == nil {
					sps = naluData
				}
			case NALTypePPS:
				if pps == nil {
					pps = naluData
				}
			}
			offset += naluLen
		}
	}
	return vps, sps, pps, nil
}
