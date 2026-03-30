package webrtc

import (
	"log/slog"
	"sync"

	"github.com/pion/webrtc/v4"
)

// Session represents an active WebRTC peer connection for WHIP or WHEP.
type Session struct {
	id        string
	pc        *webrtc.PeerConnection
	streamKey string
	role      string // "whip" or "whep"
	module    *Module
	done      chan struct{}
	closeOnce sync.Once
}

// newSession creates a new WebRTC session.
func newSession(id string, pc *webrtc.PeerConnection, streamKey, role string, m *Module) *Session {
	s := &Session{
		id:        id,
		pc:        pc,
		streamKey: streamKey,
		role:      role,
		module:    m,
		done:      make(chan struct{}),
	}

	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		slog.Debug("ICE state", "module", "webrtc", "session", id, "state", state)
		if state == webrtc.ICEConnectionStateFailed || state == webrtc.ICEConnectionStateClosed {
			s.Close()
		}
	})

	return s
}

// Close shuts down the session, closing the PeerConnection and cleaning up resources.
func (s *Session) Close() {
	s.closeOnce.Do(func() {
		close(s.done)
		s.pc.Close()
		s.module.removeSession(s)
		slog.Info("session closed", "module", "webrtc", "session", s.id, "role", s.role, "stream", s.streamKey)
	})
}
