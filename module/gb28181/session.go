package gb28181

import (
	"net"
	"sync"

	"github.com/im-pingo/liveforge/core"
)

// MediaSession tracks the state of a GB28181 media session.
type MediaSession struct {
	mu        sync.Mutex
	ID        string           // SIP Call-ID
	DeviceID  string
	ChannelID string
	StreamKey string
	Direction SessionDirection
	LocalPort int
	RemoteAddr *net.UDPAddr
	Transport  string // "udp" or "tcp"
	State      SessionState
	Publisher  *Publisher
	Stream     *core.Stream
	SSRC       uint32
}

// SetState transitions the session to a new state.
func (s *MediaSession) SetState(state SessionState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.State = state
}

// GetState returns the current session state.
func (s *MediaSession) GetState() SessionState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.State
}

// Close terminates the media session.
func (s *MediaSession) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.State = SessionStateClosed
	if s.Publisher != nil {
		s.Publisher.Close()
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
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[session.ID] = session
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

// GetByStreamKey finds a session by stream key.
func (m *SessionManager) GetByStreamKey(key string) *MediaSession {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, s := range m.sessions {
		if s.StreamKey == key {
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
		if s.ChannelID == channelID && s.State != SessionStateClosed {
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
		if s.DeviceID == deviceID {
			s.Close()
			delete(m.sessions, id)
		}
	}
}
