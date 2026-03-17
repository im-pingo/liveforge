package h264

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
)

// NAL unit types.
const (
	NALTypeSPS = 7
	NALTypePPS = 8
	NALTypeIDR = 5
)

// SPSInfo holds parsed SPS information.
type SPSInfo struct {
	Width   int
	Height  int
	Profile int
	Level   int
}

// ParseSPS parses an H.264 SPS NAL unit and extracts resolution and profile info.
// The input should include the NAL header byte (0x67).
func ParseSPS(data []byte) (*SPSInfo, error) {
	if len(data) < 4 {
		return nil, errors.New("SPS data too short")
	}

	// Remove emulation prevention bytes (0x00 0x00 0x03 → 0x00 0x00)
	rbsp := removeEmulationPrevention(data)

	r := newBitReader(rbsp)

	// NAL header: forbidden_zero_bit(1), nal_ref_idc(2), nal_unit_type(5)
	_ = r.readBits(8)

	profileIDC := r.readBits(8)
	_ = r.readBits(8) // constraint_set flags + reserved
	levelIDC := r.readBits(8)

	_ = r.readUE() // seq_parameter_set_id

	chromaFormatIDC := 1 // default
	if profileIDC == 100 || profileIDC == 110 || profileIDC == 122 ||
		profileIDC == 244 || profileIDC == 44 || profileIDC == 83 ||
		profileIDC == 86 || profileIDC == 118 || profileIDC == 128 ||
		profileIDC == 138 || profileIDC == 139 || profileIDC == 134 ||
		profileIDC == 135 {
		chromaFormatIDC = r.readUE()
		if chromaFormatIDC == 3 {
			_ = r.readBits(1) // separate_colour_plane_flag
		}
		_ = r.readUE() // bit_depth_luma_minus8
		_ = r.readUE() // bit_depth_chroma_minus8
		_ = r.readBits(1) // qpprime_y_zero_transform_bypass_flag
		scalingMatrixPresent := r.readBits(1)
		if scalingMatrixPresent != 0 {
			count := 8
			if chromaFormatIDC == 3 {
				count = 12
			}
			for i := 0; i < count; i++ {
				if r.readBits(1) != 0 { // scaling_list_present_flag
					size := 16
					if i >= 6 {
						size = 64
					}
					skipScalingList(r, size)
				}
			}
		}
	}

	_ = r.readUE() // log2_max_frame_num_minus4
	picOrderCntType := r.readUE()
	if picOrderCntType == 0 {
		_ = r.readUE() // log2_max_pic_order_cnt_lsb_minus4
	} else if picOrderCntType == 1 {
		_ = r.readBits(1) // delta_pic_order_always_zero_flag
		_ = r.readSE()    // offset_for_non_ref_pic
		_ = r.readSE()    // offset_for_top_to_bottom_field
		numRefFrames := r.readUE()
		for i := 0; i < numRefFrames; i++ {
			_ = r.readSE() // offset_for_ref_frame
		}
	}

	_ = r.readUE()    // max_num_ref_frames
	_ = r.readBits(1) // gaps_in_frame_num_value_allowed_flag

	picWidthInMbs := r.readUE() + 1
	picHeightInMapUnits := r.readUE() + 1
	frameMbsOnly := r.readBits(1)

	if frameMbsOnly == 0 {
		_ = r.readBits(1) // mb_adaptive_frame_field_flag
	}

	_ = r.readBits(1) // direct_8x8_inference_flag

	frameCropping := r.readBits(1)
	cropLeft, cropRight, cropTop, cropBottom := 0, 0, 0, 0
	if frameCropping != 0 {
		cropLeft = r.readUE()
		cropRight = r.readUE()
		cropTop = r.readUE()
		cropBottom = r.readUE()
	}

	if r.err != nil {
		return nil, fmt.Errorf("SPS parse error: %w", r.err)
	}

	width := picWidthInMbs * 16
	height := (2 - frameMbsOnly) * picHeightInMapUnits * 16

	// Apply cropping
	cropUnitX, cropUnitY := 1, 2-frameMbsOnly
	if chromaFormatIDC == 1 {
		cropUnitX = 2
		cropUnitY *= 2
	} else if chromaFormatIDC == 2 {
		cropUnitX = 2
	}
	width -= (cropLeft + cropRight) * cropUnitX
	height -= (cropTop + cropBottom) * cropUnitY

	return &SPSInfo{
		Width:   width,
		Height:  height,
		Profile: profileIDC,
		Level:   levelIDC,
	}, nil
}

// ExtractNALUs splits Annex-B byte stream into individual NAL units.
// It handles both 3-byte (0x00 0x00 0x01) and 4-byte (0x00 0x00 0x00 0x01) start codes.
func ExtractNALUs(data []byte) [][]byte {
	var nalus [][]byte
	start := -1

	i := 0
	for i < len(data) {
		// Look for start code
		if i+2 < len(data) && data[i] == 0x00 && data[i+1] == 0x00 {
			startCodeLen := 0
			if i+2 < len(data) && data[i+2] == 0x01 {
				startCodeLen = 3
			} else if i+3 < len(data) && data[i+2] == 0x00 && data[i+3] == 0x01 {
				startCodeLen = 4
			}

			if startCodeLen > 0 {
				if start >= 0 {
					nalus = append(nalus, data[start:i])
				}
				start = i + startCodeLen
				i += startCodeLen
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

// AVCCToAnnexB converts AVCC format (4-byte big-endian length-prefixed NALUs)
// to Annex-B format (0x00000001 start code prefixed NALUs).
func AVCCToAnnexB(data []byte) []byte {
	startCode := []byte{0x00, 0x00, 0x00, 0x01}
	var result []byte
	offset := 0
	for offset+4 <= len(data) {
		naluLen := int(binary.BigEndian.Uint32(data[offset : offset+4]))
		offset += 4
		if offset+naluLen > len(data) {
			break
		}
		result = append(result, startCode...)
		result = append(result, data[offset:offset+naluLen]...)
		offset += naluLen
	}
	return result
}

// ExtractSPSPPSFromAVCRecord parses an AVCDecoderConfigurationRecord and
// extracts the first SPS and first PPS NAL units.
func ExtractSPSPPSFromAVCRecord(data []byte) (sps, pps []byte, err error) {
	if len(data) < 7 {
		return nil, nil, errors.New("AVC record too short")
	}

	offset := 5 // skip configurationVersion, profile, compat, level, lengthSizeMinusOne
	numSPS := int(data[offset] & 0x1F)
	offset++

	for i := 0; i < numSPS; i++ {
		if offset+2 > len(data) {
			return nil, nil, errors.New("AVC record truncated reading SPS length")
		}
		spsLen := int(binary.BigEndian.Uint16(data[offset : offset+2]))
		offset += 2
		if offset+spsLen > len(data) {
			return nil, nil, errors.New("AVC record truncated reading SPS data")
		}
		if i == 0 {
			sps = make([]byte, spsLen)
			copy(sps, data[offset:offset+spsLen])
		}
		offset += spsLen
	}

	if offset >= len(data) {
		return nil, nil, errors.New("AVC record truncated before PPS")
	}
	numPPS := int(data[offset])
	offset++

	for i := 0; i < numPPS; i++ {
		if offset+2 > len(data) {
			return nil, nil, errors.New("AVC record truncated reading PPS length")
		}
		ppsLen := int(binary.BigEndian.Uint16(data[offset : offset+2]))
		offset += 2
		if offset+ppsLen > len(data) {
			return nil, nil, errors.New("AVC record truncated reading PPS data")
		}
		if i == 0 {
			pps = make([]byte, ppsLen)
			copy(pps, data[offset:offset+ppsLen])
		}
		offset += ppsLen
	}

	if sps == nil {
		return nil, nil, errors.New("no SPS found in AVC record")
	}
	if pps == nil {
		return nil, nil, errors.New("no PPS found in AVC record")
	}
	return sps, pps, nil
}

func skipScalingList(r *bitReader, size int) {
	lastScale := 8
	nextScale := 8
	for j := 0; j < size; j++ {
		if nextScale != 0 {
			delta := r.readSE()
			nextScale = (lastScale + delta + 256) % 256
		}
		if nextScale != 0 {
			lastScale = nextScale
		}
	}
}

func removeEmulationPrevention(data []byte) []byte {
	// Fast path: if no emulation prevention bytes, return as-is
	if !bytes.Contains(data, []byte{0x00, 0x00, 0x03}) {
		return data
	}

	result := make([]byte, 0, len(data))
	for i := 0; i < len(data); i++ {
		if i+2 < len(data) && data[i] == 0x00 && data[i+1] == 0x00 && data[i+2] == 0x03 {
			result = append(result, 0x00, 0x00)
			i += 2 // skip the 0x03
		} else {
			result = append(result, data[i])
		}
	}
	return result
}

// bitReader reads bits from a byte slice.
type bitReader struct {
	data   []byte
	offset int // bit offset
	err    error
}

func newBitReader(data []byte) *bitReader {
	return &bitReader{data: data}
}

func (r *bitReader) readBits(n int) int {
	if r.err != nil {
		return 0
	}
	val := 0
	for i := 0; i < n; i++ {
		byteIdx := r.offset / 8
		bitIdx := 7 - (r.offset % 8)
		if byteIdx >= len(r.data) {
			r.err = errors.New("bit reader: out of data")
			return 0
		}
		val = (val << 1) | int((r.data[byteIdx]>>uint(bitIdx))&1)
		r.offset++
	}
	return val
}

// readUE reads an unsigned Exp-Golomb coded value.
func (r *bitReader) readUE() int {
	if r.err != nil {
		return 0
	}
	leadingZeros := 0
	for r.readBits(1) == 0 {
		if r.err != nil {
			return 0
		}
		leadingZeros++
		if leadingZeros > 31 {
			r.err = errors.New("bit reader: excessive leading zeros in exp-golomb")
			return 0
		}
	}
	if leadingZeros == 0 {
		return 0
	}
	val := (1 << uint(leadingZeros)) - 1 + r.readBits(leadingZeros)
	return val
}

// readSE reads a signed Exp-Golomb coded value.
func (r *bitReader) readSE() int {
	ue := r.readUE()
	if ue%2 == 0 {
		return -(ue / 2)
	}
	return (ue + 1) / 2
}
