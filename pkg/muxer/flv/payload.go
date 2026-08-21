package flv

import (
	"errors"
	"fmt"

	"github.com/im-pingo/liveforge/pkg/avframe"
)

// ErrUnsupportedPacket identifies an E-RTMP packet that cannot be represented
// by the single-track AVFrame model.
var ErrUnsupportedPacket = errors.New("flv: unsupported packet")

// ParseVideoPayload parses a classic or enhanced FLV video tag body.
func ParseVideoPayload(data []byte, dts int64) (*avframe.AVFrame, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("video payload is empty")
	}
	if data[0]&0x80 != 0 {
		return parseEnhancedVideoPayload(data, dts)
	}
	return parseClassicVideoPayload(data, dts)
}

func parseClassicVideoPayload(data []byte, dts int64) (*avframe.AVFrame, error) {
	if len(data) < 2 {
		return nil, fmt.Errorf("classic video payload too short: %d", len(data))
	}

	codec := FLVVideoCodecToAVFrame(data[0] & 0x0f)
	if codec == 0 {
		return nil, fmt.Errorf("unsupported classic video codec ID: %d", data[0]&0x0f)
	}
	packetType := data[1]
	if packetType == AVCPacketEndOfSequence {
		return nil, nil
	}
	if packetType != AVCPacketSequenceHeader && packetType != AVCPacketNALU {
		return nil, fmt.Errorf("%w: classic video packet type %d", ErrUnsupportedPacket, packetType)
	}
	if len(data) < 5 {
		return nil, fmt.Errorf("classic video payload too short for CTS: %d", len(data))
	}

	frameType := frameTypeFromFLV(data[0] >> 4)
	if packetType == AVCPacketSequenceHeader {
		frameType = avframe.FrameTypeSequenceHeader
	}
	return avframe.NewAVFrame(
		avframe.MediaTypeVideo,
		codec,
		frameType,
		dts,
		dts+decodeSI24(data[2:5]),
		copyPayload(data[5:]),
	), nil
}

func parseEnhancedVideoPayload(data []byte, dts int64) (*avframe.AVFrame, error) {
	if len(data) < 5 {
		return nil, fmt.Errorf("enhanced video payload too short: %d", len(data))
	}

	codec := VideoFourCCToCodec(string(data[1:5]))
	if codec == 0 {
		return nil, fmt.Errorf("%w: video FourCC %q", ErrUnsupportedPacket, string(data[1:5]))
	}
	packetType := data[0] & 0x0f
	frameType := frameTypeFromFLV((data[0] >> 4) & 0x07)
	offset := 5
	var cts int64

	switch packetType {
	case ExVideoPacketSequenceStart:
		frameType = avframe.FrameTypeSequenceHeader
	case ExVideoPacketCodedFrames:
		if codec == avframe.CodecH264 || codec == avframe.CodecH265 {
			if len(data) < 8 {
				return nil, fmt.Errorf("enhanced video payload too short for CTS: %d", len(data))
			}
			cts = decodeSI24(data[5:8])
			offset = 8
		}
	case ExVideoPacketCodedFramesX:
		// Composition offset is implicitly zero.
	case ExVideoPacketSequenceEnd:
		return nil, nil
	default:
		return nil, fmt.Errorf("%w: enhanced video packet type %d", ErrUnsupportedPacket, packetType)
	}

	return avframe.NewAVFrame(
		avframe.MediaTypeVideo,
		codec,
		frameType,
		dts,
		dts+cts,
		copyPayload(data[offset:]),
	), nil
}

// ParseAudioPayload parses a classic or enhanced FLV audio tag body.
func ParseAudioPayload(data []byte, dts int64) (*avframe.AVFrame, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("audio payload is empty")
	}
	if data[0]>>4 == AudioFormatExHeader {
		return parseEnhancedAudioPayload(data, dts)
	}
	return parseClassicAudioPayload(data, dts)
}

func parseClassicAudioPayload(data []byte, dts int64) (*avframe.AVFrame, error) {
	formatID := data[0] >> 4
	codec := FLVAudioCodecToAVFrame(formatID)
	if codec == 0 {
		return nil, fmt.Errorf("unsupported classic audio format ID: %d", formatID)
	}

	offset := 1
	frameType := avframe.FrameTypeInterframe
	if codec == avframe.CodecAAC || codec == avframe.CodecOpus {
		if len(data) < 2 {
			return nil, fmt.Errorf("classic audio payload too short for packet type: %d", len(data))
		}
		packetType := data[1]
		switch packetType {
		case AACPacketSequenceHeader:
			frameType = avframe.FrameTypeSequenceHeader
		case AACPacketRaw:
		default:
			return nil, fmt.Errorf("%w: classic audio packet type %d", ErrUnsupportedPacket, packetType)
		}
		offset = 2
	}

	return avframe.NewAVFrame(
		avframe.MediaTypeAudio,
		codec,
		frameType,
		dts,
		dts,
		copyPayload(data[offset:]),
	), nil
}

func parseEnhancedAudioPayload(data []byte, dts int64) (*avframe.AVFrame, error) {
	if len(data) < 5 {
		return nil, fmt.Errorf("enhanced audio payload too short: %d", len(data))
	}

	codec := AudioFourCCToCodec(string(data[1:5]))
	if codec == 0 {
		return nil, fmt.Errorf("%w: audio FourCC %q", ErrUnsupportedPacket, string(data[1:5]))
	}
	packetType := data[0] & 0x0f
	frameType := avframe.FrameTypeInterframe
	switch packetType {
	case ExAudioPacketSequenceStart:
		frameType = avframe.FrameTypeSequenceHeader
	case ExAudioPacketCodedFrames:
	case ExAudioPacketSequenceEnd:
		return nil, nil
	default:
		return nil, fmt.Errorf("%w: enhanced audio packet type %d", ErrUnsupportedPacket, packetType)
	}

	return avframe.NewAVFrame(
		avframe.MediaTypeAudio,
		codec,
		frameType,
		dts,
		dts,
		copyPayload(data[5:]),
	), nil
}

func frameTypeFromFLV(frameType uint8) avframe.FrameType {
	if frameType == VideoFrameKeyframe {
		return avframe.FrameTypeKeyframe
	}
	return avframe.FrameTypeInterframe
}

func copyPayload(payload []byte) []byte {
	return append([]byte(nil), payload...)
}
