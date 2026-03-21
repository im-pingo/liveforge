package rtp

import (
	"crypto/rand"
	"encoding/binary"

	pionrtp "github.com/pion/rtp/v2"
)

// Session tracks RTP session state: SSRC, sequence numbers, and timestamps.
type Session struct {
	SSRC        uint32
	PayloadType uint8
	ClockRate   uint32
	seqNum      uint16
}

// NewSession creates a new RTP session with a random SSRC.
func NewSession(pt uint8, clockRate uint32) *Session {
	var buf [4]byte
	rand.Read(buf[:])
	return &Session{
		SSRC:        binary.BigEndian.Uint32(buf[:]),
		PayloadType: pt,
		ClockRate:   clockRate,
	}
}

// WrapPackets fills in SSRC, PayloadType, Timestamp, and sequential SequenceNumbers
// for the given packets. dtsMsec is the decode timestamp in milliseconds.
func (s *Session) WrapPackets(pkts []*pionrtp.Packet, dtsMsec int64) []*pionrtp.Packet {
	ts := uint32(dtsMsec * int64(s.ClockRate) / 1000)
	for _, pkt := range pkts {
		pkt.Version = 2
		pkt.SSRC = s.SSRC
		pkt.PayloadType = s.PayloadType
		pkt.Timestamp = ts
		pkt.SequenceNumber = s.seqNum
		s.seqNum++
	}
	return pkts
}
