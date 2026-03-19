package rtp

import (
	"fmt"

	"github.com/im-pingo/liveforge/pkg/avframe"
	pionrtp "github.com/pion/rtp/v2"
)

// MP3Packetizer packetizes MP3 frames into RTP packets per RFC 2250.
// Each packet carries a 4-byte header (2 bytes MBZ + 2 bytes fragment offset)
// followed by the MP3 frame data.
type MP3Packetizer struct{}

// Packetize wraps an MP3 AVFrame into a single RTP packet with RFC 2250 header.
func (p *MP3Packetizer) Packetize(frame *avframe.AVFrame, mtu int) ([]*pionrtp.Packet, error) {
	if len(frame.Payload) == 0 {
		return nil, fmt.Errorf("empty MP3 frame")
	}

	// RFC 2250: 4-byte header = 2 bytes MBZ (0x0000) + 2 bytes fragment offset (0x0000 for full frames)
	payload := make([]byte, 4+len(frame.Payload))
	// First 4 bytes are already zero (MBZ + fragment offset = 0)
	copy(payload[4:], frame.Payload)

	pkt := &pionrtp.Packet{
		Header: pionrtp.Header{
			Marker: true,
		},
		Payload: payload,
	}

	return []*pionrtp.Packet{pkt}, nil
}

// MP3Depacketizer reassembles RTP packets into MP3 AVFrames.
type MP3Depacketizer struct{}

// Depacketize extracts an MP3 frame from an RTP packet, stripping the RFC 2250 header.
func (d *MP3Depacketizer) Depacketize(pkt *pionrtp.Packet) (*avframe.AVFrame, error) {
	if len(pkt.Payload) <= 4 {
		return nil, fmt.Errorf("MP3 RTP payload too short: %d bytes", len(pkt.Payload))
	}

	// Skip 4-byte RFC 2250 header
	mp3Data := make([]byte, len(pkt.Payload)-4)
	copy(mp3Data, pkt.Payload[4:])

	return &avframe.AVFrame{
		MediaType: avframe.MediaTypeAudio,
		Codec:     avframe.CodecMP3,
		Payload:   mp3Data,
	}, nil
}
