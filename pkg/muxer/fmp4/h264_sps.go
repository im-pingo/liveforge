package fmp4

import "encoding/binary"

// ParseAVCCDimensions extracts width and height from an AVCDecoderConfigurationRecord
// (the raw content of an avcC box). Returns 0,0 if parsing fails.
func ParseAVCCDimensions(avcc []byte) (width, height int) {
	if len(avcc) < 8 {
		return
	}
	// AVCDecoderConfigurationRecord:
	// [0] configurationVersion
	// [1] AVCProfileIndication
	// [2] profile_compatibility
	// [3] AVCLevelIndication
	// [4] lengthSizeMinusOne | 0xFC
	// [5] numSPS | 0xE0
	// [6:8] SPS NALU length (big-endian)
	// [8:8+spsLen] SPS NALU (first byte = NAL unit type = 0x67)
	numSPS := int(avcc[5] & 0x1F)
	if numSPS == 0 {
		return
	}
	if len(avcc) < 8 {
		return
	}
	spsLen := int(binary.BigEndian.Uint16(avcc[6:8]))
	if len(avcc) < 8+spsLen || spsLen < 2 {
		return
	}
	// SPS NALU: skip NAL header byte (0x67), parse RBSP
	spsNALU := avcc[8 : 8+spsLen]
	return parseSPSDimensions(spsNALU[1:]) // skip 0x67 NAL type byte
}

// parseSPSDimensions parses H.264 SPS RBSP bytes (after NAL type byte) to extract
// coded width and height. Handles the emulation_prevention_three_byte removal.
func parseSPSDimensions(rbsp []byte) (width, height int) {
	// Remove emulation prevention bytes (0x00 0x00 0x03 → 0x00 0x00)
	cleaned := removeEPB(rbsp)
	if len(cleaned) < 4 {
		return
	}

	br := &bitReader{data: cleaned}

	// profile_idc (8 bits)
	profileIDC := br.readBits(8)
	// constraint_set flags (8 bits)
	br.readBits(8)
	// level_idc (8 bits)
	br.readBits(8)

	// seq_parameter_set_id
	br.readUE()

	// High-profile extensions
	if profileIDC == 100 || profileIDC == 110 || profileIDC == 122 || profileIDC == 244 ||
		profileIDC == 44 || profileIDC == 83 || profileIDC == 86 || profileIDC == 118 ||
		profileIDC == 128 || profileIDC == 138 || profileIDC == 139 || profileIDC == 134 {
		chromaFormatIDC := br.readUE()
		if chromaFormatIDC == 3 {
			br.readBits(1) // separate_colour_plane_flag
		}
		br.readUE() // bit_depth_luma_minus8
		br.readUE() // bit_depth_chroma_minus8
		br.readBits(1) // qpprime_y_zero_transform_bypass_flag
		seqScalingMatrixPresentFlag := br.readBits(1)
		if seqScalingMatrixPresentFlag != 0 {
			n := 8
			if chromaFormatIDC == 3 {
				n = 12
			}
			for i := 0; i < n; i++ {
				if br.readBits(1) != 0 { // seq_scaling_list_present_flag
					// skip scaling list
					listSize := 16
					if i >= 6 {
						listSize = 64
					}
					lastScale, nextScale := 8, 8
					for j := 0; j < listSize; j++ {
						if nextScale != 0 {
							deltaScale := br.readSE()
							nextScale = (lastScale + deltaScale + 256) % 256
						}
						if nextScale != 0 {
							lastScale = nextScale
						}
					}
				}
			}
		}
	}

	br.readUE() // log2_max_frame_num_minus4
	picOrderCntType := br.readUE()
	if picOrderCntType == 0 {
		br.readUE() // log2_max_pic_order_cnt_lsb_minus4
	} else if picOrderCntType == 1 {
		br.readBits(1)  // delta_pic_order_always_zero_flag
		br.readSE()     // offset_for_non_ref_pic
		br.readSE()     // offset_for_top_to_bottom_field
		n := br.readUE() // num_ref_frames_in_pic_order_cnt_cycle
		for i := 0; i < n; i++ {
			br.readSE() // offset_for_ref_frame[i]
		}
	}

	br.readUE() // num_ref_frames
	br.readBits(1) // gaps_in_frame_num_value_allowed_flag

	if br.err {
		return
	}

	picWidthInMBsMinus1 := br.readUE()
	picHeightInMapUnitsMinus1 := br.readUE()
	frameMBsOnlyFlag := br.readBits(1)

	if br.err {
		return
	}

	if frameMBsOnlyFlag == 0 {
		br.readBits(1) // mb_adaptive_frame_field_flag
	}

	br.readBits(1) // direct_8x8_inference_flag

	cropLeft, cropRight, cropTop, cropBottom := 0, 0, 0, 0
	if br.readBits(1) != 0 { // frame_cropping_flag
		cropLeft = br.readUE()
		cropRight = br.readUE()
		cropTop = br.readUE()
		cropBottom = br.readUE()
	}

	if br.err {
		return
	}

	w := (picWidthInMBsMinus1+1)*16 - (cropLeft+cropRight)*2
	h := (picHeightInMapUnitsMinus1+1)*16*(2-frameMBsOnlyFlag) - (cropTop+cropBottom)*2*(2-frameMBsOnlyFlag)
	if w > 0 && h > 0 {
		width, height = w, h
	}
	return
}

// removeEPB removes H.264 emulation prevention bytes (0x00 0x00 0x03 → 0x00 0x00).
func removeEPB(src []byte) []byte {
	dst := make([]byte, 0, len(src))
	for i := 0; i < len(src); {
		if i+2 < len(src) && src[i] == 0x00 && src[i+1] == 0x00 && src[i+2] == 0x03 {
			dst = append(dst, 0x00, 0x00)
			i += 3
		} else {
			dst = append(dst, src[i])
			i++
		}
	}
	return dst
}

// bitReader reads bits from a byte slice (MSB first).
type bitReader struct {
	data   []byte
	bitPos int
	err    bool
}

func (br *bitReader) readBits(n int) int {
	if br.err {
		return 0
	}
	result := 0
	for i := 0; i < n; i++ {
		byteIdx := br.bitPos / 8
		bitIdx := 7 - (br.bitPos % 8)
		if byteIdx >= len(br.data) {
			br.err = true
			return 0
		}
		bit := int((br.data[byteIdx] >> uint(bitIdx)) & 1)
		result = (result << 1) | bit
		br.bitPos++
	}
	return result
}

// readUE reads an unsigned Exp-Golomb coded integer.
func (br *bitReader) readUE() int {
	leadingZeros := 0
	for {
		bit := br.readBits(1)
		if br.err {
			return 0
		}
		if bit != 0 {
			break
		}
		leadingZeros++
		if leadingZeros > 31 {
			br.err = true
			return 0
		}
	}
	if leadingZeros == 0 {
		return 0
	}
	info := br.readBits(leadingZeros)
	return (1 << leadingZeros) - 1 + info
}

// readSE reads a signed Exp-Golomb coded integer.
func (br *bitReader) readSE() int {
	codeNum := br.readUE()
	if codeNum%2 == 0 {
		return -(codeNum / 2)
	}
	return (codeNum + 1) / 2
}
