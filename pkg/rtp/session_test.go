package rtp

import (
	"testing"

	pionrtp "github.com/pion/rtp/v2"
)

func TestNewSession(t *testing.T) {
	s := NewSession(96, 90000)
	if s.SSRC == 0 {
		t.Error("SSRC should be non-zero random")
	}
	if s.PayloadType != 96 {
		t.Errorf("PayloadType = %d", s.PayloadType)
	}
	if s.ClockRate != 90000 {
		t.Errorf("ClockRate = %d", s.ClockRate)
	}
}

func TestSessionWrapPackets(t *testing.T) {
	s := NewSession(96, 90000)
	pkts := []*pionrtp.Packet{
		{Header: pionrtp.Header{}},
		{Header: pionrtp.Header{}},
	}
	wrapped := s.WrapPackets(pkts, 1000)
	if len(wrapped) != 2 {
		t.Fatalf("len = %d", len(wrapped))
	}
	if wrapped[0].SSRC != s.SSRC || wrapped[1].SSRC != s.SSRC {
		t.Error("SSRC mismatch")
	}
	if wrapped[0].PayloadType != 96 {
		t.Errorf("PT = %d", wrapped[0].PayloadType)
	}
	if wrapped[1].SequenceNumber != wrapped[0].SequenceNumber+1 {
		t.Error("seq not sequential")
	}
	// Timestamp: 1000ms * 90000 / 1000 = 90000
	if wrapped[0].Timestamp != 90000 {
		t.Errorf("Timestamp = %d, want 90000", wrapped[0].Timestamp)
	}
}
