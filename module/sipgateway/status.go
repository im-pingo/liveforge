package sipgateway

import (
	"context"
	"errors"
	"sync/atomic"
	"time"
)

var (
	// ErrCallNotFound indicates that the requested call is no longer active.
	ErrCallNotFound = errors.New("SIP gateway call not found")
	// ErrStreamNotFound indicates that an outbound call referenced an unknown stream.
	ErrStreamNotFound = errors.New("SIP gateway stream not found")
	// ErrCallCapacity indicates that the configured concurrent-call limit was reached.
	ErrCallCapacity = errors.New("SIP gateway maximum calls reached")
	// ErrGatewayClosed indicates that the gateway is shutting down.
	ErrGatewayClosed = errors.New("SIP gateway is closed")
	// ErrTargetRequired indicates that an outbound call omitted its SIP target.
	ErrTargetRequired = errors.New("SIP gateway target is required")
	// ErrPortExhausted indicates that no RTP/RTCP pair is available.
	ErrPortExhausted = errors.New("SIP gateway RTP ports exhausted")
	// ErrCodecMismatch indicates that the remote side selected no configured codec.
	ErrCodecMismatch = errors.New("SIP gateway codec mismatch")
)

// SIPGatewayProvider is the control-plane contract exposed by Module.
type SIPGatewayProvider interface {
	ListCalls() []CallSnapshot
	Call(callID string) (CallSnapshot, bool)
	Dial(ctx context.Context, targetURI, streamKey string) (string, error)
	Hangup(callID string) error
	Metrics() MetricsSnapshot
}

// CallState describes the lifecycle state exposed by the gateway control plane.
type CallState string

const (
	CallStateEstablishing CallState = "establishing"
	CallStateActive       CallState = "active"
	CallStateEnded        CallState = "ended"
	CallStateNetworkLost  CallState = "network_lost"
)

// CallSnapshot is an immutable point-in-time view of a SIP gateway call.
type CallSnapshot struct {
	CallID         string    `json:"call_id"`
	Direction      string    `json:"direction"`
	StreamKey      string    `json:"stream_key"`
	Codec          string    `json:"codec"`
	RTPPort        int       `json:"rtp_port"`
	RTCPPort       int       `json:"rtcp_port"`
	RemoteAddress  string    `json:"remote_address,omitempty"`
	StartedAt      time.Time `json:"started_at"`
	State          CallState `json:"state"`
	LastError      string    `json:"last_error,omitempty"`
	LastRTPAt      time.Time `json:"last_rtp_at,omitempty"`
	RTPPacketsSent uint64    `json:"rtp_packets_sent"`
	RTPPacketsRecv uint64    `json:"rtp_packets_received"`
	RTPBytesSent   uint64    `json:"rtp_bytes_sent"`
	RTPBytesRecv   uint64    `json:"rtp_bytes_received"`
}

// MetricsSnapshot contains bounded-cardinality gateway counters for metrics
// collectors and management APIs.
type MetricsSnapshot struct {
	ActiveCalls        int    `json:"active_calls"`
	ActiveInbound      int    `json:"active_inbound"`
	ActiveOutbound     int    `json:"active_outbound"`
	CallsStarted       uint64 `json:"calls_started_total"`
	CallsEnded         uint64 `json:"calls_ended_total"`
	SetupFailures      uint64 `json:"setup_failures_total"`
	CodecFailures      uint64 `json:"codec_failures_total"`
	DuplicateCallIDs   uint64 `json:"duplicate_call_ids_total"`
	PortExhaustions    uint64 `json:"port_exhaustions_total"`
	CapacityRejections uint64 `json:"capacity_rejections_total"`
	NetworkFailures    uint64 `json:"network_failures_total"`
	RTPPacketsSent     uint64 `json:"rtp_packets_sent_total"`
	RTPPacketsRecv     uint64 `json:"rtp_packets_received_total"`
	RTPBytesSent       uint64 `json:"rtp_bytes_sent_total"`
	RTPBytesRecv       uint64 `json:"rtp_bytes_received_total"`
}

type gatewayMetrics struct {
	callsStarted       atomic.Uint64
	callsEnded         atomic.Uint64
	setupFailures      atomic.Uint64
	codecFailures      atomic.Uint64
	duplicateCallIDs   atomic.Uint64
	portExhaustions    atomic.Uint64
	capacityRejections atomic.Uint64
	networkFailures    atomic.Uint64
	rtpPacketsSent     atomic.Uint64
	rtpPacketsRecv     atomic.Uint64
	rtpBytesSent       atomic.Uint64
	rtpBytesRecv       atomic.Uint64
}

func (m *gatewayMetrics) snapshot() MetricsSnapshot {
	return MetricsSnapshot{
		CallsStarted:       m.callsStarted.Load(),
		CallsEnded:         m.callsEnded.Load(),
		SetupFailures:      m.setupFailures.Load(),
		CodecFailures:      m.codecFailures.Load(),
		DuplicateCallIDs:   m.duplicateCallIDs.Load(),
		PortExhaustions:    m.portExhaustions.Load(),
		CapacityRejections: m.capacityRejections.Load(),
		NetworkFailures:    m.networkFailures.Load(),
		RTPPacketsSent:     m.rtpPacketsSent.Load(),
		RTPPacketsRecv:     m.rtpPacketsRecv.Load(),
		RTPBytesSent:       m.rtpBytesSent.Load(),
		RTPBytesRecv:       m.rtpBytesRecv.Load(),
	}
}
