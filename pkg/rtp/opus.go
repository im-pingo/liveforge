package rtp

import (
	"fmt"

	"github.com/im-pingo/liveforge/pkg/avframe"
	pionrtp "github.com/pion/rtp/v2"
)

// OpusPacketizer packetizes Opus frames into RTP packets.
// Opus frames map directly to RTP payloads with no additional framing.
type OpusPacketizer struct{}

// Packetize wraps an Opus AVFrame into a single RTP packet.
func (p *OpusPacketizer) Packetize(frame *avframe.AVFrame, mtu int) ([]*pionrtp.Packet, error) {
	if len(frame.Payload) == 0 {
		return nil, fmt.Errorf("empty Opus frame")
	}

	payload := make([]byte, len(frame.Payload))
	copy(payload, frame.Payload)

	pkt := &pionrtp.Packet{
		Header: pionrtp.Header{
			Marker: true,
		},
		Payload: payload,
	}

	return []*pionrtp.Packet{pkt}, nil
}

// OpusDepacketizer reassembles RTP packets into Opus AVFrames.
type OpusDepacketizer struct{}

// Depacketize extracts an Opus frame from an RTP packet.
func (d *OpusDepacketizer) Depacketize(pkt *pionrtp.Packet) (*avframe.AVFrame, error) {
	if len(pkt.Payload) == 0 {
		return nil, fmt.Errorf("empty Opus RTP payload")
	}

	payload := make([]byte, len(pkt.Payload))
	copy(payload, pkt.Payload)

	return &avframe.AVFrame{
		MediaType: avframe.MediaTypeAudio,
		Codec:     avframe.CodecOpus,
		Payload:   payload,
	}, nil
}
