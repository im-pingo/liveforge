package fmp4

import (
	"encoding/binary"

	"github.com/im-pingo/liveforge/pkg/codec/h264"
)

// ParseAVCCDimensions extracts width and height from an AVCDecoderConfigurationRecord
// (the raw content of an avcC box). Returns 0,0 if parsing fails.
func ParseAVCCDimensions(avcc []byte) (width, height int) {
	if len(avcc) < 8 || avcc[0] != 1 || avcc[5]&0x1f == 0 {
		return 0, 0
	}
	spsLen := int(binary.BigEndian.Uint16(avcc[6:8]))
	if spsLen > len(avcc)-8 {
		return 0, 0
	}
	info, err := h264.ParseSPS(avcc[8 : 8+spsLen])
	if err != nil {
		return 0, 0
	}
	return info.Width, info.Height
}
