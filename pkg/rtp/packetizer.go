package rtp

import (
	"fmt"

	"github.com/im-pingo/liveforge/pkg/avframe"
	pionrtp "github.com/pion/rtp/v2"
)

// DefaultMTU is the default Maximum Transmission Unit for RTP packets.
const DefaultMTU = 1400

// Packetizer splits an AVFrame into RTP packets.
type Packetizer interface {
	Packetize(frame *avframe.AVFrame, mtu int) ([]*pionrtp.Packet, error)
}

// Depacketizer reassembles RTP packets into AVFrames.
type Depacketizer interface {
	Depacketize(pkt *pionrtp.Packet) (*avframe.AVFrame, error)
}

// NewPacketizer creates a Packetizer for the given codec.
func NewPacketizer(codec avframe.CodecType) (Packetizer, error) {
	switch codec {
	case avframe.CodecH264:
		return &H264Packetizer{}, nil
	case avframe.CodecH265:
		return &H265Packetizer{}, nil
	case avframe.CodecAAC:
		return &AACPacketizer{}, nil
	case avframe.CodecOpus:
		return &OpusPacketizer{}, nil
	case avframe.CodecG711U:
		return &G711Packetizer{}, nil
	case avframe.CodecG711A:
		return &G711Packetizer{}, nil
	case avframe.CodecMP3:
		return &MP3Packetizer{}, nil
	case avframe.CodecG722:
		return &G722Packetizer{}, nil
	case avframe.CodecG729:
		return &G729Packetizer{}, nil
	case avframe.CodecSpeex:
		return &SpeexPacketizer{}, nil
	case avframe.CodecVP8:
		return &VP8Packetizer{}, nil
	case avframe.CodecVP9:
		return &VP9Packetizer{}, nil
	case avframe.CodecAV1:
		return &AV1Packetizer{}, nil
	default:
		return nil, fmt.Errorf("unsupported codec for packetizer: %v", codec)
	}
}

// NewDepacketizer creates a Depacketizer for the given codec.
func NewDepacketizer(codec avframe.CodecType) (Depacketizer, error) {
	switch codec {
	case avframe.CodecH264:
		return &H264Depacketizer{}, nil
	case avframe.CodecH265:
		return &H265Depacketizer{}, nil
	case avframe.CodecAAC:
		return &AACDepacketizer{}, nil
	case avframe.CodecOpus:
		return &OpusDepacketizer{}, nil
	case avframe.CodecG711U:
		return &G711Depacketizer{Codec: avframe.CodecG711U}, nil
	case avframe.CodecG711A:
		return &G711Depacketizer{Codec: avframe.CodecG711A}, nil
	case avframe.CodecMP3:
		return &MP3Depacketizer{}, nil
	case avframe.CodecG722:
		return &G722Depacketizer{}, nil
	case avframe.CodecG729:
		return &G729Depacketizer{}, nil
	case avframe.CodecSpeex:
		return &SpeexDepacketizer{}, nil
	case avframe.CodecVP8:
		return &VP8Depacketizer{}, nil
	case avframe.CodecVP9:
		return &VP9Depacketizer{}, nil
	case avframe.CodecAV1:
		return &AV1Depacketizer{}, nil
	default:
		return nil, fmt.Errorf("unsupported codec for depacketizer: %v", codec)
	}
}

