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
// containing VPS, SPS, and PPS NAL units. Returns nil if SPS is absent or invalid.
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
	spsInfo, ok := parseSPSHVCCInfo(sps)
	if !ok {
		return nil
	}

	// 22-byte fixed header + arrays
	config := make([]byte, 22)
	config[0] = 1 // configurationVersion
	copy(config[1:13], spsInfo.profileTierLevel[:])
	config[13] = 0xF0 // reserved + minSpatialSegmentationIDC=0
	config[15] = 0xFC // reserved + parallelismType=0
	config[16] = 0xFC | spsInfo.chromaFormatIDC
	config[17] = 0xF8 | spsInfo.bitDepthLumaMinus8
	config[18] = 0xF8 | spsInfo.bitDepthChromaMinus8
	config[21] = (spsInfo.numTemporalLayers << 3) | (spsInfo.temporalIDNested << 2) | 0x03

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

type spsHVCCInfo struct {
	profileTierLevel     [12]byte
	chromaFormatIDC      byte
	bitDepthLumaMinus8   byte
	bitDepthChromaMinus8 byte
	numTemporalLayers    byte
	temporalIDNested     byte
	width                int
	height               int
}

func parseSPSHVCCInfo(sps []byte) (spsHVCCInfo, bool) {
	var info spsHVCCInfo
	if len(sps) < 3 || (sps[0]>>1)&0x3F != NALTypeSPS {
		return info, false
	}

	rbsp := removeEmulationPreventionBytes(sps[2:])
	if len(rbsp) < 13 {
		return info, false
	}

	maxSubLayersMinus1 := (rbsp[0] >> 1) & 0x07
	if maxSubLayersMinus1 > 6 {
		return info, false
	}
	info.numTemporalLayers = maxSubLayersMinus1 + 1
	info.temporalIDNested = rbsp[0] & 0x01
	copy(info.profileTierLevel[:], rbsp[1:13])

	reader := h265BitReader{data: rbsp, bitPos: 13 * 8}
	profilePresent := make([]bool, int(maxSubLayersMinus1))
	levelPresent := make([]bool, int(maxSubLayersMinus1))
	if maxSubLayersMinus1 > 0 {
		for i := range profilePresent {
			profilePresent[i] = reader.readBits(1) != 0
			levelPresent[i] = reader.readBits(1) != 0
		}
		for i := int(maxSubLayersMinus1); i < 8; i++ {
			reader.readBits(2) // reserved_zero_2bits
		}
		for i := range profilePresent {
			if profilePresent[i] {
				reader.readBits(88)
			}
			if levelPresent[i] {
				reader.readBits(8)
			}
		}
	}

	reader.readUE() // sps_seq_parameter_set_id
	chromaFormatIDC := reader.readUE()
	if chromaFormatIDC > 3 {
		return info, false
	}
	separateColourPlane := uint64(0)
	if chromaFormatIDC == 3 {
		separateColourPlane = reader.readBits(1)
	}
	width := reader.readUE()
	height := reader.readUE()
	var cropLeft, cropRight, cropTop, cropBottom uint64
	if reader.readBits(1) != 0 {
		cropLeft = reader.readUE()
		cropRight = reader.readUE()
		cropTop = reader.readUE()
		cropBottom = reader.readUE()
	}
	bitDepthLumaMinus8 := reader.readUE()
	bitDepthChromaMinus8 := reader.readUE()
	if reader.err || width == 0 || height == 0 || bitDepthLumaMinus8 > 7 || bitDepthChromaMinus8 > 7 {
		return info, false
	}

	subWidthC, subHeightC := uint64(1), uint64(1)
	if separateColourPlane == 0 {
		switch chromaFormatIDC {
		case 1:
			subWidthC, subHeightC = 2, 2
		case 2:
			subWidthC = 2
		}
	}
	cropWidth := subWidthC * (cropLeft + cropRight)
	cropHeight := subHeightC * (cropTop + cropBottom)
	if cropWidth >= width || cropHeight >= height || width-cropWidth > 0xffff || height-cropHeight > 0xffff {
		return info, false
	}

	info.chromaFormatIDC = byte(chromaFormatIDC)
	info.bitDepthLumaMinus8 = byte(bitDepthLumaMinus8)
	info.bitDepthChromaMinus8 = byte(bitDepthChromaMinus8)
	info.width = int(width - cropWidth)
	info.height = int(height - cropHeight)
	return info, true
}

// ParseHVCCDimensions extracts the display dimensions from the first SPS in an
// HEVCDecoderConfigurationRecord. It returns zero values for malformed input.
func ParseHVCCDimensions(config []byte) (width, height int) {
	_, sps, _, err := ExtractVPSSPSPPSFromHVCRecord(config)
	if err != nil || sps == nil {
		return 0, 0
	}
	info, ok := parseSPSHVCCInfo(sps)
	if !ok {
		return 0, 0
	}
	return info.width, info.height
}

func removeEmulationPreventionBytes(data []byte) []byte {
	result := make([]byte, 0, len(data))
	for i := 0; i < len(data); i++ {
		if i+2 < len(data) && data[i] == 0x00 && data[i+1] == 0x00 && data[i+2] == 0x03 {
			result = append(result, 0x00, 0x00)
			i += 2
			continue
		}
		result = append(result, data[i])
	}
	return result
}

type h265BitReader struct {
	data   []byte
	bitPos int
	err    bool
}

func (r *h265BitReader) readBits(n int) uint64 {
	if r.err || n < 0 || n > 64 {
		r.err = true
		return 0
	}
	var value uint64
	for i := 0; i < n; i++ {
		byteIndex := r.bitPos / 8
		if byteIndex >= len(r.data) {
			r.err = true
			return 0
		}
		bitIndex := 7 - (r.bitPos % 8)
		value = (value << 1) | uint64((r.data[byteIndex]>>uint(bitIndex))&1)
		r.bitPos++
	}
	return value
}

func (r *h265BitReader) readUE() uint64 {
	leadingZeros := 0
	for r.readBits(1) == 0 {
		if r.err {
			return 0
		}
		leadingZeros++
		if leadingZeros > 31 {
			r.err = true
			return 0
		}
	}
	if leadingZeros == 0 {
		return 0
	}
	return (uint64(1)<<uint(leadingZeros) - 1) + r.readBits(leadingZeros)
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
			return nil, nil, nil, fmt.Errorf("HEVCDecoderConfigurationRecord array %d header is truncated", i)
		}
		nalType := data[offset] & 0x3F
		offset++
		if offset+2 > len(data) {
			return nil, nil, nil, fmt.Errorf("HEVCDecoderConfigurationRecord array %d NAL count is truncated", i)
		}
		numNalus := int(binary.BigEndian.Uint16(data[offset:]))
		offset += 2
		for j := 0; j < numNalus; j++ {
			if offset+2 > len(data) {
				return nil, nil, nil, fmt.Errorf("HEVCDecoderConfigurationRecord array %d NAL %d length is truncated", i, j)
			}
			naluLen := int(binary.BigEndian.Uint16(data[offset:]))
			offset += 2
			if offset+naluLen > len(data) {
				return nil, nil, nil, fmt.Errorf("HEVCDecoderConfigurationRecord array %d NAL %d payload is truncated", i, j)
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
