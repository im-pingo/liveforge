package rtp

import (
	"fmt"

	"github.com/im-pingo/liveforge/pkg/avframe"
	pionrtp "github.com/pion/rtp/v2"
)

// G722Packetizer packetizes G.722 frames into RTP packets.
// G.722 samples map directly to RTP payloads with no additional framing.
type G722Packetizer struct{}

// Packetize wraps a G.722 AVFrame into a single RTP packet.
func (p *G722Packetizer) Packetize(frame *avframe.AVFrame, mtu int) ([]*pionrtp.Packet, error) {
	if len(frame.Payload) == 0 {
		return nil, fmt.Errorf("empty G.722 frame")
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

// G722Depacketizer reassembles RTP packets into G.722 AVFrames.
type G722Depacketizer struct{}

// Depacketize extracts a G.722 frame from an RTP packet.
func (d *G722Depacketizer) Depacketize(pkt *pionrtp.Packet) (*avframe.AVFrame, error) {
	if len(pkt.Payload) == 0 {
		return nil, fmt.Errorf("empty G.722 RTP payload")
	}

	payload := make([]byte, len(pkt.Payload))
	copy(payload, pkt.Payload)

	return &avframe.AVFrame{
		MediaType: avframe.MediaTypeAudio,
		Codec:     avframe.CodecG722,
		Payload:   payload,
	}, nil
}
