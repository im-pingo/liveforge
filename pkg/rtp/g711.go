package rtp

import (
	"fmt"

	"github.com/im-pingo/liveforge/pkg/avframe"
	pionrtp "github.com/pion/rtp/v2"
)

// G711Packetizer packetizes G.711 (PCMU/PCMA) frames into RTP packets.
// G.711 samples map directly to RTP payloads with no additional framing.
type G711Packetizer struct{}

// Packetize wraps a G.711 AVFrame into a single RTP packet.
func (p *G711Packetizer) Packetize(frame *avframe.AVFrame, mtu int) ([]*pionrtp.Packet, error) {
	if len(frame.Payload) == 0 {
		return nil, fmt.Errorf("empty G.711 frame")
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

// G711Depacketizer reassembles RTP packets into G.711 AVFrames.
type G711Depacketizer struct {
	// Codec stores the specific G.711 variant (CodecG711U or CodecG711A).
	Codec avframe.CodecType
}

// Depacketize extracts a G.711 frame from an RTP packet.
func (d *G711Depacketizer) Depacketize(pkt *pionrtp.Packet) (*avframe.AVFrame, error) {
	if len(pkt.Payload) == 0 {
		return nil, fmt.Errorf("empty G.711 RTP payload")
	}

	payload := make([]byte, len(pkt.Payload))
	copy(payload, pkt.Payload)

	return &avframe.AVFrame{
		MediaType: avframe.MediaTypeAudio,
		Codec:     d.Codec,
		Payload:   payload,
	}, nil
}
