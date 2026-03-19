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
	default:
		return nil, fmt.Errorf("unsupported codec for packetizer: %v", codec)
	}
}

// NewDepacketizer creates a Depacketizer for the given codec.
func NewDepacketizer(codec avframe.CodecType) (Depacketizer, error) {
	switch codec {
	case avframe.CodecH264:
		return &H264Depacketizer{}, nil
	default:
		return nil, fmt.Errorf("unsupported codec for depacketizer: %v", codec)
	}
}

// H264Packetizer is a stub packetizer for H.264 — replaced with real implementation in Task 6.
type H264Packetizer struct{}

func (p *H264Packetizer) Packetize(frame *avframe.AVFrame, mtu int) ([]*pionrtp.Packet, error) {
	return nil, fmt.Errorf("H264Packetizer not yet implemented")
}

// H264Depacketizer is a stub depacketizer for H.264 — replaced with real implementation in Task 6.
type H264Depacketizer struct{}

func (d *H264Depacketizer) Depacketize(pkt *pionrtp.Packet) (*avframe.AVFrame, error) {
	return nil, fmt.Errorf("H264Depacketizer not yet implemented")
}
