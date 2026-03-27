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

// AnnexBToHVCC converts Annex-B format (start codes) to HVCC format (4-byte length prefix).
func AnnexBToHVCC(data []byte) []byte {
	var result []byte
	nalus := extractNALUs(data)
	for _, nal := range nalus {
		if len(nal) == 0 {
			continue
		}
		var lenBuf [4]byte
		binary.BigEndian.PutUint32(lenBuf[:], uint32(len(nal)))
		result = append(result, lenBuf[:]...)
		result = append(result, nal...)
	}
	return result
}

// BuildHVCCDecoderConfig builds an HEVCDecoderConfigurationRecord from Annex-B data
// containing VPS, SPS, and PPS NAL units. Returns nil if SPS not found.
func BuildHVCCDecoderConfig(annexB []byte) []byte {
	var vps, sps, pps []byte
	nalus := extractNALUs(annexB)
	for _, nal := range nalus {
		if len(nal) < 2 {
			continue
		}
		nalType := (nal[0] >> 1) & 0x3F
		switch nalType {
		case NALTypeVPS:
			if vps == nil {
				vps = nal
			}
		case NALTypeSPS:
			if sps == nil {
				sps = nal
			}
		case NALTypePPS:
			if pps == nil {
				pps = nal
			}
		}
	}
	if sps == nil {
		return nil
	}
	return buildHVCCFromNALs(vps, sps, pps)
}

// buildHVCCFromNALs builds an HEVCDecoderConfigurationRecord from raw NAL bytes.
func buildHVCCFromNALs(vps, sps, pps []byte) []byte {
	// 22-byte fixed header + arrays
	config := make([]byte, 22)
	config[0] = 1    // configurationVersion
	config[21] = 0x03 // lengthSizeMinusOne=3 | 2 reserved bits

	numArrays := byte(0)
	var arrays []byte

	addArray := func(nalType byte, nal []byte) {
		if nal == nil {
			return
		}
		numArrays++
		arrays = append(arrays, nalType) // array_completeness=0 | nal_unit_type
		var buf [2]byte
		binary.BigEndian.PutUint16(buf[:], 1) // numNalus=1
		arrays = append(arrays, buf[:]...)
		binary.BigEndian.PutUint16(buf[:], uint16(len(nal)))
		arrays = append(arrays, buf[:]...)
		arrays = append(arrays, nal...)
	}

	addArray(NALTypeVPS, vps)
	addArray(NALTypeSPS, sps)
	addArray(NALTypePPS, pps)

	config = append(config, numArrays)
	config = append(config, arrays...)
	return config
}

// extractNALUs splits Annex-B byte stream into individual NAL units.
func extractNALUs(data []byte) [][]byte {
	var nalus [][]byte
	start := -1
	i := 0
	for i < len(data) {
		if i+2 < len(data) && data[i] == 0x00 && data[i+1] == 0x00 {
			scLen := 0
			if data[i+2] == 0x01 {
				scLen = 3
			} else if i+3 < len(data) && data[i+2] == 0x00 && data[i+3] == 0x01 {
				scLen = 4
			}
			if scLen > 0 {
				if start >= 0 {
					nalus = append(nalus, data[start:i])
				}
				start = i + scLen
				i += scLen
				continue
			}
		}
		i++
	}
	if start >= 0 && start < len(data) {
		nalus = append(nalus, data[start:])
	}
	return nalus
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
