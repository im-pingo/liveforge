package flv

import "github.com/im-pingo/liveforge/pkg/avframe"

// FLV tag types.
const (
	TagTypeAudio  uint8 = 8
	TagTypeVideo  uint8 = 9
	TagTypeScript uint8 = 18
)

// FLV video frame types (upper 4 bits of first video data byte).
const (
	VideoFrameKeyframe   uint8 = 1
	VideoFrameInterframe uint8 = 2
)

// FLV video codec IDs (lower 4 bits of first video data byte).
const (
	VideoCodecH264 uint8 = 7
	VideoCodecH265 uint8 = 12 // Enhanced RTMP
	VideoCodecAV1  uint8 = 13 // Enhanced RTMP
)

// Enhanced FLV FourCC codes.
var (
	FourCCAVC  = [4]byte{'a', 'v', 'c', '1'}
	FourCCHEVC = [4]byte{'h', 'v', 'c', '1'}
	FourCCAV1  = [4]byte{'a', 'v', '0', '1'}
	FourCCVP9  = [4]byte{'v', 'p', '0', '9'}
)

// Enhanced video packet types (ExVideoTagHeader).
const (
	ExVideoPacketSequenceStart  uint8 = 0
	ExVideoPacketCodedFrames    uint8 = 1
	ExVideoPacketSequenceEnd    uint8 = 2
	ExVideoPacketCodedFramesX   uint8 = 3
)

// IsEnhancedVideoCodec returns true for codecs that use the Enhanced FLV format.
func IsEnhancedVideoCodec(c avframe.CodecType) bool {
	return c == avframe.CodecH265 || c == avframe.CodecAV1 || c == avframe.CodecVP9
}

// VideoCodecToFourCC returns the FourCC for an enhanced video codec.
func VideoCodecToFourCC(c avframe.CodecType) [4]byte {
	switch c {
	case avframe.CodecH264:
		return FourCCAVC
	case avframe.CodecH265:
		return FourCCHEVC
	case avframe.CodecAV1:
		return FourCCAV1
	case avframe.CodecVP9:
		return FourCCVP9
	default:
		return [4]byte{}
	}
}

// FLV audio format IDs (upper 4 bits of first audio data byte).
const (
	AudioFormatAAC  uint8 = 10
	AudioFormatOpus uint8 = 13 // Enhanced RTMP
	AudioFormatMP3  uint8 = 2
)

// AVC packet types (second byte of video data for H.264).
const (
	AVCPacketSequenceHeader uint8 = 0
	AVCPacketNALU           uint8 = 1
	AVCPacketEndOfSequence  uint8 = 2
)

// AAC packet types (second byte of audio data for AAC).
const (
	AACPacketSequenceHeader uint8 = 0
	AACPacketRaw            uint8 = 1
)

// FLV header (9 bytes): "FLV" + version + flags + header length.
var FLVHeader = []byte{0x46, 0x4C, 0x56, 0x01, 0x05, 0x00, 0x00, 0x00, 0x09}

// FLV tag header size.
const TagHeaderSize = 11

// PreviousTagSize0 is the 4-byte zero following the FLV header.
var PreviousTagSize0 = []byte{0x00, 0x00, 0x00, 0x00}

// AVFrameTypeToFLV converts an AVFrame frame type to FLV video frame type.
func AVFrameTypeToFLV(ft avframe.FrameType) uint8 {
	if ft == avframe.FrameTypeKeyframe || ft == avframe.FrameTypeSequenceHeader {
		return VideoFrameKeyframe
	}
	return VideoFrameInterframe
}

// VideoCodecToFLV converts an AVFrame codec to FLV video codec ID.
func VideoCodecToFLV(c avframe.CodecType) uint8 {
	switch c {
	case avframe.CodecH264:
		return VideoCodecH264
	case avframe.CodecH265:
		return VideoCodecH265
	case avframe.CodecAV1:
		return VideoCodecAV1
	default:
		return 0
	}
}

// AudioCodecToFLV converts an AVFrame codec to FLV audio format ID.
func AudioCodecToFLV(c avframe.CodecType) uint8 {
	switch c {
	case avframe.CodecAAC:
		return AudioFormatAAC
	case avframe.CodecOpus:
		return AudioFormatOpus
	case avframe.CodecMP3:
		return AudioFormatMP3
	default:
		return 0
	}
}

// FLVVideoCodecToAVFrame converts FLV video codec ID to AVFrame codec.
func FLVVideoCodecToAVFrame(id uint8) avframe.CodecType {
	switch id {
	case VideoCodecH264:
		return avframe.CodecH264
	case VideoCodecH265:
		return avframe.CodecH265
	case VideoCodecAV1:
		return avframe.CodecAV1
	default:
		return 0
	}
}

// FLVAudioCodecToAVFrame converts FLV audio format ID to AVFrame codec.
func FLVAudioCodecToAVFrame(id uint8) avframe.CodecType {
	switch id {
	case AudioFormatAAC:
		return avframe.CodecAAC
	case AudioFormatOpus:
		return avframe.CodecOpus
	case AudioFormatMP3:
		return avframe.CodecMP3
	default:
		return 0
	}
}
