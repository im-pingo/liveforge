package flv

import (
	"fmt"

	"github.com/im-pingo/liveforge/pkg/avframe"
)

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
	VideoCodecH265 uint8 = 12
	VideoCodecAV1  uint8 = 13
)

// Enhanced FLV FourCC codes.
var (
	FourCCVP8  = [4]byte{'v', 'p', '0', '8'}
	FourCCVP9  = [4]byte{'v', 'p', '0', '9'}
	FourCCAVC  = [4]byte{'a', 'v', 'c', '1'}
	FourCCHEVC = [4]byte{'h', 'v', 'c', '1'}
	FourCCAV1  = [4]byte{'a', 'v', '0', '1'}
)

// Enhanced audio FourCC codes.
var (
	FourCCAAC  = [4]byte{'m', 'p', '4', 'a'}
	FourCCOpus = [4]byte{'O', 'p', 'u', 's'}
	FourCCMP3  = [4]byte{'.', 'm', 'p', '3'}
)

// Enhanced video packet types (ExVideoTagHeader).
const (
	ExVideoPacketSequenceStart uint8 = 0
	ExVideoPacketCodedFrames   uint8 = 1
	ExVideoPacketSequenceEnd   uint8 = 2
	ExVideoPacketCodedFramesX  uint8 = 3
	ExVideoPacketMetadata      uint8 = 4
	ExVideoPacketMPEG2TSStart  uint8 = 5
	ExVideoPacketMultitrack    uint8 = 6
	ExVideoPacketModEx         uint8 = 7
)

// Enhanced audio packet types.
const (
	ExAudioPacketSequenceStart uint8 = 0
	ExAudioPacketCodedFrames   uint8 = 1
	ExAudioPacketSequenceEnd   uint8 = 2
	ExAudioPacketMultichannel  uint8 = 4
	ExAudioPacketMultitrack    uint8 = 5
	ExAudioPacketModEx         uint8 = 7
)

// EncodingMode selects the media header format emitted by Muxer.
type EncodingMode uint8

const (
	EncodingAuto EncodingMode = iota
	EncodingClassic
	EncodingEnhanced
)

// IsEnhancedVideoCodec returns true when the codec has a Phase 1 FourCC form.
func IsEnhancedVideoCodec(c avframe.CodecType) bool {
	return VideoFourCC(c) != ""
}

// IsEnhancedAudioCodec returns true when the codec has a Phase 1 FourCC form.
func IsEnhancedAudioCodec(c avframe.CodecType) bool {
	return AudioFourCC(c) != ""
}

// VideoCodecToFourCC returns the FourCC for an enhanced video codec.
func VideoCodecToFourCC(c avframe.CodecType) [4]byte {
	switch c {
	case avframe.CodecVP8:
		return FourCCVP8
	case avframe.CodecVP9:
		return FourCCVP9
	case avframe.CodecH264:
		return FourCCAVC
	case avframe.CodecH265:
		return FourCCHEVC
	case avframe.CodecAV1:
		return FourCCAV1
	default:
		return [4]byte{}
	}
}

// VideoFourCC returns the FourCC string for a video codec.
func VideoFourCC(c avframe.CodecType) string {
	bytes := VideoCodecToFourCC(c)
	return string(bytes[:])
}

// AudioCodecToFourCC returns the FourCC string for an audio codec.
func AudioCodecToFourCC(c avframe.CodecType) string {
	switch c {
	case avframe.CodecAAC:
		return string(FourCCAAC[:])
	case avframe.CodecOpus:
		return string(FourCCOpus[:])
	case avframe.CodecMP3:
		return string(FourCCMP3[:])
	default:
		return ""
	}
}

// AudioFourCC returns the FourCC string for an audio codec.
func AudioFourCC(c avframe.CodecType) string {
	return AudioCodecToFourCC(c)
}

// VideoFourCCToCodec converts an enhanced video FourCC to an AVFrame codec.
func VideoFourCCToCodec(fourCC string) avframe.CodecType {
	switch fourCC {
	case string(FourCCVP8[:]):
		return avframe.CodecVP8
	case string(FourCCVP9[:]):
		return avframe.CodecVP9
	case string(FourCCAVC[:]):
		return avframe.CodecH264
	case string(FourCCHEVC[:]):
		return avframe.CodecH265
	case string(FourCCAV1[:]):
		return avframe.CodecAV1
	default:
		return 0
	}
}

// AudioFourCCToCodec converts an enhanced audio FourCC to an AVFrame codec.
func AudioFourCCToCodec(fourCC string) avframe.CodecType {
	switch fourCC {
	case string(FourCCAAC[:]):
		return avframe.CodecAAC
	case string(FourCCOpus[:]):
		return avframe.CodecOpus
	case string(FourCCMP3[:]):
		return avframe.CodecMP3
	default:
		return 0
	}
}

func encodeSI24(value int64) ([3]byte, error) {
	if value < -8388608 || value > 8388607 {
		return [3]byte{}, fmt.Errorf("signed SI24 overflow: %d", value)
	}
	encoded := uint32(value) & 0x00ffffff
	return [3]byte{byte(encoded >> 16), byte(encoded >> 8), byte(encoded)}, nil
}

func decodeSI24(data []byte) int64 {
	value := int32(uint32(data[0])<<16 | uint32(data[1])<<8 | uint32(data[2]))
	if value&0x00800000 != 0 {
		value |= ^int32(0x00ffffff)
	}
	return int64(value)
}

// FLV audio format IDs (upper 4 bits of first audio data byte).
const (
	AudioFormatAAC      uint8 = 10
	AudioFormatExHeader uint8 = 9
	AudioFormatOpus     uint8 = 13 // Legacy draft value retained for callers.
	AudioFormatMP3      uint8 = 2
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
