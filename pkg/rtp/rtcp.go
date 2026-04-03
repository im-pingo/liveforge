package rtp

import (
	"encoding/binary"
	"fmt"
)

// RTCP packet type constants (RFC 3550).
const (
	RTCPTypeSR  = 200
	RTCPTypeRR  = 201
	RTCPTypeBYE = 203
)

const (
	rtcpVersion = 2
	rtcpPTSR    = RTCPTypeSR
	rtcpPTRR    = RTCPTypeRR
	rtcpPTBYE   = RTCPTypeBYE

	rtcpHeaderSize      = 4
	rtcpSRBodySize      = 24 // SSRC(4) + NTP(8) + RTPts(4) + pktCount(4) + octetCount(4)
	rtcpSRSize          = 28 // header(4) + SSRC(4) + NTP(8) + RTPts(4) + pktCount(4) + octetCount(4)
	rtcpRRHeaderSize    = 8  // header(4) + SSRC(4)
	rtcpReportBlockSize = 24
	rtcpBYESize         = 8 // header(4) + SSRC(4)
)

// SenderReport represents an RTCP Sender Report (PT=200).
type SenderReport struct {
	SSRC        uint32
	NTPTime     uint64
	RTPTime     uint32
	PacketCount uint32
	OctetCount  uint32
}

// ReceiverReport represents an RTCP Receiver Report (PT=201).
type ReceiverReport struct {
	SSRC    uint32
	Reports []ReceptionReport
}

// ReceptionReport represents a single reception report block within an RR.
type ReceptionReport struct {
	SSRC             uint32
	FractionLost     uint8
	TotalLost        uint32 // 24-bit value
	HighestSeq       uint32
	Jitter           uint32
	LastSR           uint32
	DelaySinceLastSR uint32
}

// BuildSR builds an RTCP Sender Report packet per RFC 3550.
func BuildSR(ssrc uint32, ntpTime uint64, rtpTime, packetCount, octetCount uint32) []byte {
	buf := make([]byte, rtcpSRSize)
	// Header: V=2, P=0, RC=0, PT=200, length in 32-bit words minus one
	// length = (28/4) - 1 = 6
	buf[0] = rtcpVersion << 6
	buf[1] = rtcpPTSR
	binary.BigEndian.PutUint16(buf[2:4], 6) // length in 32-bit words - 1

	binary.BigEndian.PutUint32(buf[4:8], ssrc)
	binary.BigEndian.PutUint64(buf[8:16], ntpTime)
	binary.BigEndian.PutUint32(buf[16:20], rtpTime)
	binary.BigEndian.PutUint32(buf[20:24], packetCount)
	binary.BigEndian.PutUint32(buf[24:28], octetCount)
	return buf
}

// ParseSR parses an RTCP Sender Report from raw bytes.
func ParseSR(data []byte) (*SenderReport, error) {
	if len(data) < rtcpSRSize {
		return nil, fmt.Errorf("SR too short: %d bytes", len(data))
	}
	version := data[0] >> 6
	if version != rtcpVersion {
		return nil, fmt.Errorf("invalid RTCP version: %d", version)
	}
	pt := data[1]
	if pt != rtcpPTSR {
		return nil, fmt.Errorf("not a Sender Report: PT=%d", pt)
	}
	return &SenderReport{
		SSRC:        binary.BigEndian.Uint32(data[4:8]),
		NTPTime:     binary.BigEndian.Uint64(data[8:16]),
		RTPTime:     binary.BigEndian.Uint32(data[16:20]),
		PacketCount: binary.BigEndian.Uint32(data[20:24]),
		OctetCount:  binary.BigEndian.Uint32(data[24:28]),
	}, nil
}

// BuildRR builds an RTCP Receiver Report packet per RFC 3550.
func BuildRR(ssrc uint32, reports []ReceptionReport) []byte {
	rc := len(reports)
	size := rtcpRRHeaderSize + rc*rtcpReportBlockSize
	buf := make([]byte, size)

	// Header: V=2, P=0, RC=count, PT=201
	buf[0] = (rtcpVersion << 6) | uint8(rc&0x1f)
	buf[1] = rtcpPTRR
	// length in 32-bit words minus one
	binary.BigEndian.PutUint16(buf[2:4], uint16(size/4-1))

	binary.BigEndian.PutUint32(buf[4:8], ssrc)

	for i, r := range reports {
		off := rtcpRRHeaderSize + i*rtcpReportBlockSize
		binary.BigEndian.PutUint32(buf[off:off+4], r.SSRC)
		// fraction lost (8 bits) + cumulative lost (24 bits)
		buf[off+4] = r.FractionLost
		buf[off+5] = uint8((r.TotalLost >> 16) & 0xff)
		buf[off+6] = uint8((r.TotalLost >> 8) & 0xff)
		buf[off+7] = uint8(r.TotalLost & 0xff)
		binary.BigEndian.PutUint32(buf[off+8:off+12], r.HighestSeq)
		binary.BigEndian.PutUint32(buf[off+12:off+16], r.Jitter)
		binary.BigEndian.PutUint32(buf[off+16:off+20], r.LastSR)
		binary.BigEndian.PutUint32(buf[off+20:off+24], r.DelaySinceLastSR)
	}
	return buf
}

// ParseRR parses an RTCP Receiver Report from raw bytes.
func ParseRR(data []byte) (*ReceiverReport, error) {
	if len(data) < rtcpRRHeaderSize {
		return nil, fmt.Errorf("RR too short: %d bytes", len(data))
	}
	version := data[0] >> 6
	if version != rtcpVersion {
		return nil, fmt.Errorf("invalid RTCP version: %d", version)
	}
	pt := data[1]
	if pt != rtcpPTRR {
		return nil, fmt.Errorf("not a Receiver Report: PT=%d", pt)
	}
	rc := int(data[0] & 0x1f)
	expected := rtcpRRHeaderSize + rc*rtcpReportBlockSize
	if len(data) < expected {
		return nil, fmt.Errorf("RR too short for %d reports: need %d, got %d", rc, expected, len(data))
	}

	rr := &ReceiverReport{
		SSRC:    binary.BigEndian.Uint32(data[4:8]),
		Reports: make([]ReceptionReport, rc),
	}
	for i := 0; i < rc; i++ {
		off := rtcpRRHeaderSize + i*rtcpReportBlockSize
		rr.Reports[i] = ReceptionReport{
			SSRC:             binary.BigEndian.Uint32(data[off : off+4]),
			FractionLost:     data[off+4],
			TotalLost:        uint32(data[off+5])<<16 | uint32(data[off+6])<<8 | uint32(data[off+7]),
			HighestSeq:       binary.BigEndian.Uint32(data[off+8 : off+12]),
			Jitter:           binary.BigEndian.Uint32(data[off+12 : off+16]),
			LastSR:           binary.BigEndian.Uint32(data[off+16 : off+20]),
			DelaySinceLastSR: binary.BigEndian.Uint32(data[off+20 : off+24]),
		}
	}
	return rr, nil
}

// BuildBYE builds an RTCP BYE packet (PT=203) per RFC 3550.
// It signals that the given SSRC is leaving the session.
func BuildBYE(ssrc uint32) []byte {
	buf := make([]byte, rtcpBYESize)
	// Header: V=2, P=0, SC=1 (one SSRC), PT=203
	// length = (8/4) - 1 = 1
	buf[0] = (rtcpVersion << 6) | 1 // SC=1
	buf[1] = rtcpPTBYE
	binary.BigEndian.PutUint16(buf[2:4], 1) // length in 32-bit words - 1
	binary.BigEndian.PutUint32(buf[4:8], ssrc)
	return buf
}
