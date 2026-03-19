package rtp

import (
	"fmt"

	"github.com/im-pingo/liveforge/pkg/avframe"
	pionrtp "github.com/pion/rtp/v2"
)

// SpeexPacketizer packetizes Speex frames into RTP packets.
// Speex frames map directly to RTP payloads with no additional framing.
type SpeexPacketizer struct{}

// Packetize wraps a Speex AVFrame into a single RTP packet.
func (p *SpeexPacketizer) Packetize(frame *avframe.AVFrame, mtu int) ([]*pionrtp.Packet, error) {
	if len(frame.Payload) == 0 {
		return nil, fmt.Errorf("empty Speex frame")
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

// SpeexDepacketizer reassembles RTP packets into Speex AVFrames.
type SpeexDepacketizer struct{}

// Depacketize extracts a Speex frame from an RTP packet.
func (d *SpeexDepacketizer) Depacketize(pkt *pionrtp.Packet) (*avframe.AVFrame, error) {
	if len(pkt.Payload) == 0 {
		return nil, fmt.Errorf("empty Speex RTP payload")
	}

	payload := make([]byte, len(pkt.Payload))
	copy(payload, pkt.Payload)

	return &avframe.AVFrame{
		MediaType: avframe.MediaTypeAudio,
		Codec:     avframe.CodecSpeex,
		Payload:   payload,
	}, nil
}
