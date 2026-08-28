package gb28181

import (
	"context"
	"errors"
	"net"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/im-pingo/liveforge/core"
)

const gbLabTerminalErrorLimit = 256

var (
	gbSIPCredentialPattern = regexp.MustCompile(`(?i)(sips?:[^\s:@]+):[^\s@]+@`)
	gbBearerTokenPattern   = regexp.MustCompile(`(?i)(bearer\s+)[^\s]+`)
)

var (
	// ErrLabInvalidRequest indicates that a protocol lab start request is invalid.
	ErrLabInvalidRequest = errors.New("GB28181 lab request is invalid")
	// ErrLabDuplicateIdentity indicates that a lab identity is already active.
	ErrLabDuplicateIdentity = errors.New("GB28181 lab identity is already active")
	// ErrLabSessionNotFound indicates that a lab session does not exist.
	ErrLabSessionNotFound = errors.New("GB28181 lab session not found")
	// ErrLabManagerUnimplemented indicates that a standalone manager has no transport to attach to.
	ErrLabManagerUnimplemented = errors.New("GB28181 lab manager is not implemented")
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
	LabSessionStateStopped  LabSessionState = "stopped"
	LabSessionStateFailed   LabSessionState = "failed"
)

// LabSessionRequest contains the protocol-neutral portion of a GB28181 lab start.
type LabSessionRequest struct {
	Mode      LabMode `json:"mode"`
	DeviceID  string  `json:"device_id"`
	ChannelID string  `json:"channel_id"`
	StreamKey string  `json:"stream_key"`
}

// LabSessionSnapshot is an immutable point-in-time view of a GB28181 lab session.
type LabSessionSnapshot struct {
	ID              string          `json:"id"`
	Identity        string          `json:"identity"`
	DeviceID        string          `json:"device_id"`
	ChannelID       string          `json:"channel_id"`
	StreamKey       string          `json:"stream_key"`
	Mode            LabMode         `json:"mode"`
	State           LabSessionState `json:"state"`
	Direction       LabDirection    `json:"direction"`
	LastError       string          `json:"last_error,omitempty"`
	RTPPacketsSent  uint64          `json:"rtp_packets_sent"`
	RTPPacketsRecv  uint64          `json:"rtp_packets_received"`
	RTPBytesSent    uint64          `json:"rtp_bytes_sent"`
	RTPBytesRecv    uint64          `json:"rtp_bytes_received"`
	RTCPPacketsSent uint64          `json:"rtcp_packets_sent"`
	RTCPPacketsRecv uint64          `json:"rtcp_packets_received"`
	PSFramesSent    uint64          `json:"ps_frames_sent"`
	PSFramesRecv    uint64          `json:"ps_frames_received"`
	AudioFramesSent uint64          `json:"audio_frames_sent"`
	AudioFramesRecv uint64          `json:"audio_frames_received"`
	VideoFramesSent uint64          `json:"video_frames_sent"`
	VideoFramesRecv uint64          `json:"video_frames_received"`
	StartedAt       time.Time       `json:"started_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
	LastMediaAt     time.Time       `json:"last_media_at,omitempty"`
	StoppedAt       time.Time       `json:"stopped_at,omitempty"`
}

func redactedLabError(err error) string {
	if err == nil {
		return ""
	}
	message := strings.Join(strings.Fields(err.Error()), " ")
	message = gbSIPCredentialPattern.ReplaceAllString(message, `${1}:[redacted]@`)
	message = gbBearerTokenPattern.ReplaceAllString(message, `${1}[redacted]`)
	runes := []rune(message)
	if len(runes) > gbLabTerminalErrorLimit {
		return string(runes[:gbLabTerminalErrorLimit])
	}
	return message
}

// LabManager owns local GB28181 lab session lifecycle state.
type LabManager interface {
	Start(ctx context.Context, request LabSessionRequest) (LabSessionSnapshot, error)
	List() []LabSessionSnapshot
	Stop(id string) error
}

type contractLabManager struct{}

// NewLabManager returns the contract-only manager; an initialized Module owns the transport-backed manager.
func NewLabManager() LabManager { return contractLabManager{} }

func (contractLabManager) Start(_ context.Context, request LabSessionRequest) (LabSessionSnapshot, error) {
	if request.Mode != LabModePublish && request.Mode != LabModeReceive {
		return LabSessionSnapshot{}, ErrLabInvalidRequest
	}
	if strings.TrimSpace(request.DeviceID) == "" ||
		strings.TrimSpace(request.ChannelID) == "" ||
		!validGBLabStreamKey(request.StreamKey) {
		return LabSessionSnapshot{}, ErrLabInvalidRequest
	}
	return LabSessionSnapshot{}, ErrLabManagerUnimplemented
}

func (contractLabManager) List() []LabSessionSnapshot { return []LabSessionSnapshot{} }

func (contractLabManager) Stop(string) error { return ErrLabManagerUnimplemented }

// MediaSession tracks the state of a GB28181 media session.
type MediaSession struct {
	mu         sync.Mutex
	ID         string // SIP Call-ID
	DeviceID   string
	ChannelID  string
	StreamKey  string
	Direction  SessionDirection
	LocalPort  int
	RemoteAddr *net.UDPAddr
	Transport  string // "udp" or "tcp"
	State      SessionState
	Publisher  *Publisher
	Receiver   *RTPReceiver
	Sender     *outboundMediaSession
	Stream     *core.Stream
	SSRC       uint32
	Playback   bool
	InviteTx   inviteDialog
	closed     bool
	published  bool
}

// MediaSessionSnapshot is an immutable view of session state used by cleanup
// and management readers.
type MediaSessionSnapshot struct {
	ID          string
	DeviceID    string
	ChannelID   string
	StreamKey   string
	Direction   SessionDirection
	LocalPort   int
	RemoteAddr  *net.UDPAddr
	Transport   string
	State       SessionState
	Publisher   *Publisher
	PublisherID string
	Receiver    *RTPReceiver
	Sender      *outboundMediaSession
	Stream      *core.Stream
	SSRC        uint32
	Playback    bool
	InviteTx    inviteDialog
	Closed      bool
	Published   bool
}

// SetState transitions the session to a new state.
func (s *MediaSession) SetState(state SessionState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.State = state
}

// GetState returns the current session state.
func (s *MediaSession) GetState() SessionState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.State
}

// MarkPublished records that the publish lifecycle start event was emitted.
// It returns false when termination already owns the session.
func (s *MediaSession) MarkPublished() bool {
	return s.startPublishLifecycle(nil)
}

func (s *MediaSession) startPublishLifecycle(emit func()) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.published {
		return false
	}
	s.published = true
	if emit != nil {
		emit()
	}
	return true
}

func (s *MediaSession) publishLifecycleStarted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.published
}

// Close terminates the media session. The first caller owns the remaining
// module cleanup and receives true; later callers are no-ops.
func (s *MediaSession) Close() bool {
	_, closed := s.closeSnapshot()
	return closed
}

func (s *MediaSession) closeSnapshot() (MediaSessionSnapshot, bool) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return MediaSessionSnapshot{}, false
	}
	s.closed = true
	s.State = SessionStateClosed
	snapshot := s.snapshotLocked()
	s.mu.Unlock()

	if snapshot.Receiver != nil {
		snapshot.Receiver.Close()
	}
	if snapshot.Sender != nil {
		snapshot.Sender.close()
	}
	if snapshot.Publisher != nil {
		_ = snapshot.Publisher.Close()
	}
	return snapshot, true
}

// Snapshot returns a locked immutable view of the session.
func (s *MediaSession) Snapshot() MediaSessionSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshotLocked()
}

func (s *MediaSession) snapshotLocked() MediaSessionSnapshot {
	var remoteAddr *net.UDPAddr
	if s.RemoteAddr != nil {
		copy := *s.RemoteAddr
		remoteAddr = &copy
	}
	publisherID := ""
	if s.Publisher != nil {
		publisherID = s.Publisher.ID()
	}
	return MediaSessionSnapshot{
		ID:          s.ID,
		DeviceID:    s.DeviceID,
		ChannelID:   s.ChannelID,
		StreamKey:   s.StreamKey,
		Direction:   s.Direction,
		LocalPort:   s.LocalPort,
		RemoteAddr:  remoteAddr,
		Transport:   s.Transport,
		State:       s.State,
		Publisher:   s.Publisher,
		PublisherID: publisherID,
		Receiver:    s.Receiver,
		Sender:      s.Sender,
		Stream:      s.Stream,
		SSRC:        s.SSRC,
		Playback:    s.Playback,
		InviteTx:    s.InviteTx,
		Closed:      s.closed,
		Published:   s.published,
	}
}

// SessionManager manages active media sessions.
type SessionManager struct {
	mu       sync.RWMutex
	sessions map[string]*MediaSession
}

// NewSessionManager creates a new session manager.
func NewSessionManager() *SessionManager {
	return &SessionManager{
		sessions: make(map[string]*MediaSession),
	}
}

// Add registers a session by its Call-ID.
func (m *SessionManager) Add(session *MediaSession) {
	snapshot := session.Snapshot()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[snapshot.ID] = session
}

// Get returns a session by Call-ID.
func (m *SessionManager) Get(callID string) *MediaSession {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sessions[callID]
}

// Remove removes a session by Call-ID.
func (m *SessionManager) Remove(callID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, callID)
}

// RemoveIf removes only the exact session generation registered under callID.
func (m *SessionManager) RemoveIf(callID string, expected *MediaSession) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessions[callID] != expected {
		return false
	}
	delete(m.sessions, callID)
	return true
}

// GetByStreamKey finds a session by stream key.
func (m *SessionManager) GetByStreamKey(key string) *MediaSession {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, s := range m.sessions {
		if s.Snapshot().StreamKey == key {
			return s
		}
	}
	return nil
}

// GetByChannel finds active sessions for a channel.
func (m *SessionManager) GetByChannel(channelID string) []*MediaSession {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var result []*MediaSession
	for _, s := range m.sessions {
		snapshot := s.Snapshot()
		if snapshot.ChannelID == channelID && snapshot.State != SessionStateClosed {
			result = append(result, s)
		}
	}
	return result
}

// All returns all active sessions.
func (m *SessionManager) All() []*MediaSession {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]*MediaSession, 0, len(m.sessions))
	for _, s := range m.sessions {
		result = append(result, s)
	}
	return result
}

// CloseByDevice closes all sessions for a device.
func (m *SessionManager) CloseByDevice(deviceID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, s := range m.sessions {
		if s.Snapshot().DeviceID == deviceID {
			s.Close()
			delete(m.sessions, id)
		}
	}
}
