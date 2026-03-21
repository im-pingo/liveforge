package webrtc

import (
	"log"
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
		log.Printf("[webrtc] session %s ICE state: %s", id, state)
		if state == webrtc.ICEConnectionStateFailed || state == webrtc.ICEConnectionStateDisconnected {
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
		log.Printf("[webrtc] session %s closed (role=%s, stream=%s)", s.id, s.role, s.streamKey)
	})
}
