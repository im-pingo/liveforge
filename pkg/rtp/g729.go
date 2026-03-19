package rtp

import (
	"fmt"

	"github.com/im-pingo/liveforge/pkg/avframe"
	pionrtp "github.com/pion/rtp/v2"
)

// G729Packetizer packetizes G.729 frames into RTP packets.
// G.729 samples map directly to RTP payloads with no additional framing.
type G729Packetizer struct{}

// Packetize wraps a G.729 AVFrame into a single RTP packet.
func (p *G729Packetizer) Packetize(frame *avframe.AVFrame, mtu int) ([]*pionrtp.Packet, error) {
	if len(frame.Payload) == 0 {
		return nil, fmt.Errorf("empty G.729 frame")
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

// G729Depacketizer reassembles RTP packets into G.729 AVFrames.
type G729Depacketizer struct{}

// Depacketize extracts a G.729 frame from an RTP packet.
func (d *G729Depacketizer) Depacketize(pkt *pionrtp.Packet) (*avframe.AVFrame, error) {
	if len(pkt.Payload) == 0 {
		return nil, fmt.Errorf("empty G.729 RTP payload")
	}

	payload := make([]byte, len(pkt.Payload))
	copy(payload, pkt.Payload)

	return &avframe.AVFrame{
		MediaType: avframe.MediaTypeAudio,
		Codec:     avframe.CodecG729,
		Payload:   payload,
	}, nil
}
