package sipgateway

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

const (
	terminalHistoryLimit = 100
	terminalErrorLimit   = 256
)

var (
	sipCredentialPattern = regexp.MustCompile(`(?i)(sips?:[^\s:@]+):[^\s@]+@`)
	bearerTokenPattern   = regexp.MustCompile(`(?i)(bearer\s+)[^\s]+`)
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
	// ErrInvalidTargetURI indicates that an outbound SIP target is malformed.
	ErrInvalidTargetURI = errors.New("SIP gateway target URI is invalid")
	// ErrPortExhausted indicates that no RTP/RTCP pair is available.
	ErrPortExhausted = errors.New("SIP gateway RTP ports exhausted")
	// ErrCodecMismatch indicates that the remote side selected no configured codec.
	ErrCodecMismatch = errors.New("SIP gateway codec mismatch")
	// ErrLabInvalidRequest indicates that a protocol lab start request is invalid.
	ErrLabInvalidRequest = errors.New("SIP gateway lab request is invalid")
	// ErrLabDuplicateIdentity indicates that a lab identity is already active.
	ErrLabDuplicateIdentity = errors.New("SIP gateway lab identity is already active")
	// ErrLabSessionNotFound indicates that a lab session does not exist.
	ErrLabSessionNotFound = errors.New("SIP gateway lab session not found")
	// ErrLabManagerUnimplemented indicates that a standalone contract manager has
	// no SIP gateway to attach a transport session to.
	ErrLabManagerUnimplemented = errors.New("SIP gateway lab manager is not implemented")
)

// LabMode selects the direction of a local protocol lab.
type LabMode string

const (
	LabModePublish LabMode = "publish"
	LabModeReceive LabMode = "receive"
)

// LabDirection describes the media direction relative to LiveForge.
type LabDirection string

const (
	LabDirectionInbound  LabDirection = "inbound"
	LabDirectionOutbound LabDirection = "outbound"
)

// LabSessionState describes the lifecycle state of a local protocol lab.
type LabSessionState string

const (
	LabSessionStateStarting LabSessionState = "starting"
	LabSessionStateActive   LabSessionState = "active"
	// LabSessionStateContract identifies a transportless contract-only manager
	// session; it has no SIP signaling, media, sockets, or availability.
	LabSessionStateContract LabSessionState = "contract"
	LabSessionStateStopped  LabSessionState = "stopped"
	LabSessionStateFailed   LabSessionState = "failed"
)

// LabSessionRequest contains the protocol-neutral portion of a SIP lab start.
type LabSessionRequest struct {
	Mode      LabMode `json:"mode"`
	DeviceID  string  `json:"device_id"`
	StreamKey string  `json:"stream_key"`
	Codec     string  `json:"codec,omitempty"`
}

// LabSessionSnapshot is an immutable point-in-time view of a SIP lab session.
type LabSessionSnapshot struct {
	ID                  string          `json:"id"`
	Identity            string          `json:"identity"`
	DeviceID            string          `json:"device_id"`
	StreamKey           string          `json:"stream_key"`
	Mode                LabMode         `json:"mode"`
	State               LabSessionState `json:"state"`
	Direction           LabDirection    `json:"direction"`
	Codec               string          `json:"codec,omitempty"`
	LastError           string          `json:"last_error,omitempty"`
	RTPPacketsSent      uint64          `json:"rtp_packets_sent"`
	RTPPacketsRecv      uint64          `json:"rtp_packets_received"`
	AudioRTPPacketsSent uint64          `json:"audio_rtp_packets_sent"`
	AudioRTPPacketsRecv uint64          `json:"audio_rtp_packets_received"`
	VideoRTPPacketsSent uint64          `json:"video_rtp_packets_sent"`
	VideoRTPPacketsRecv uint64          `json:"video_rtp_packets_received"`
	RTPBytesSent        uint64          `json:"rtp_bytes_sent"`
	RTPBytesRecv        uint64          `json:"rtp_bytes_received"`
	RTCPPacketsSent     uint64          `json:"rtcp_packets_sent"`
	RTCPPacketsRecv     uint64          `json:"rtcp_packets_received"`
	StartedAt           time.Time       `json:"started_at"`
	UpdatedAt           time.Time       `json:"updated_at"`
	LastMediaAt         time.Time       `json:"last_media_at,omitempty"`
	StoppedAt           time.Time       `json:"stopped_at,omitempty"`
}

// LabManager owns local SIP lab session lifecycle state.
type LabManager interface {
	Start(ctx context.Context, request LabSessionRequest) (LabSessionSnapshot, error)
	List() []LabSessionSnapshot
	Stop(id string) error
}

// NewLabManager returns a lifecycle manager for callers that need the
// protocol-neutral contract without a running gateway. Gateway instances use
// their transport-backed manager internally.
func NewLabManager() LabManager { return newLabManager(nil) }

// SIPGatewayProvider is the control-plane contract exposed by Module.
type SIPGatewayProvider interface {
	ListCalls() []CallSnapshot
	Call(callID string) (CallSnapshot, bool)
	Dial(ctx context.Context, targetURI, streamKey string) (string, error)
	Hangup(callID string) error
	StartLabSession(ctx context.Context, request LabSessionRequest) (LabSessionSnapshot, error)
	ListLabSessions() []LabSessionSnapshot
	StopLabSession(id string) error
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
	VideoCodec     string    `json:"video_codec,omitempty"`
	VideoRTPPort   int       `json:"video_rtp_port,omitempty"`
	VideoRTCPPort  int       `json:"video_rtcp_port,omitempty"`
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

func redactedTerminalError(err error) string {
	if err == nil {
		return ""
	}
	message := strings.Join(strings.Fields(err.Error()), " ")
	message = sipCredentialPattern.ReplaceAllString(message, `${1}:[redacted]@`)
	message = bearerTokenPattern.ReplaceAllString(message, `${1}[redacted]`)
	runes := []rune(message)
	if len(runes) > terminalErrorLimit {
		message = string(runes[:terminalErrorLimit])
	}
	return message
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
