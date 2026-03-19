package rtp

import (
	"encoding/binary"
	"fmt"

	"github.com/im-pingo/liveforge/pkg/avframe"
	pionrtp "github.com/pion/rtp/v2"
)

// AACPacketizer packetizes AAC frames into RTP packets using RFC 3640 AAC-hbr mode.
type AACPacketizer struct{}

// Packetize splits an AAC AVFrame into RTP packets.
//
// RFC 3640 AAC-hbr mode layout:
//
//	[AU-headers-length (2 bytes, value in bits)]
//	[AU-header (2 bytes: 13-bit AU-size | 3-bit AU-Index)]
//	[AAC frame data]
//
// For a single AAC frame the AU-headers-length is 16 (one 16-bit AU-header).
func (p *AACPacketizer) Packetize(frame *avframe.AVFrame, mtu int) ([]*pionrtp.Packet, error) {
	if len(frame.Payload) == 0 {
		return nil, fmt.Errorf("empty AAC frame")
	}

	frameSize := len(frame.Payload)

	// AU-headers-length: number of bits in the AU-headers section.
	// One AU-header is 16 bits, so AU-headers-length = 16 = 0x0010.
	auHeadersLength := uint16(16)

	// AU-header: 13-bit AU-size + 3-bit AU-Index (0 for single frame).
	auHeader := uint16(frameSize << 3)

	payload := make([]byte, 4+frameSize)
	binary.BigEndian.PutUint16(payload[0:2], auHeadersLength)
	binary.BigEndian.PutUint16(payload[2:4], auHeader)
	copy(payload[4:], frame.Payload)

	pkt := &pionrtp.Packet{
		Header: pionrtp.Header{
			Marker: true,
		},
		Payload: payload,
	}

	return []*pionrtp.Packet{pkt}, nil
}

// AACDepacketizer reassembles RTP packets into AAC AVFrames.
type AACDepacketizer struct{}

// Depacketize extracts an AAC frame from an RTP packet.
func (d *AACDepacketizer) Depacketize(pkt *pionrtp.Packet) (*avframe.AVFrame, error) {
	payload := pkt.Payload
	if len(payload) < 4 {
		return nil, fmt.Errorf("AAC RTP payload too short: %d bytes", len(payload))
	}

	// Read AU-headers-length (in bits) and compute header bytes to skip.
	auHeadersLengthBits := binary.BigEndian.Uint16(payload[0:2])
	auHeadersBytes := (int(auHeadersLengthBits) + 7) / 8 // round up to bytes

	headerSize := 2 + auHeadersBytes // 2 bytes for AU-headers-length + AU-headers
	if len(payload) < headerSize {
		return nil, fmt.Errorf("AAC RTP payload too short for AU-headers: need %d, got %d", headerSize, len(payload))
	}

	aacData := make([]byte, len(payload)-headerSize)
	copy(aacData, payload[headerSize:])

	return &avframe.AVFrame{
		MediaType: avframe.MediaTypeAudio,
		Codec:     avframe.CodecAAC,
		Payload:   aacData,
	}, nil
}
