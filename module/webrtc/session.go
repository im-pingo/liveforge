package webrtc

import (
	"log/slog"
	"sync"

	"github.com/im-pingo/liveforge/core"
	"github.com/pion/webrtc/v4"
)

type sessionLifecycleState uint8

const (
	sessionLifecycleInitial sessionLifecycleState = iota
	sessionLifecycleStarted
	sessionLifecycleStopped
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

	lifecycleMu    sync.Mutex
	lifecycleState sessionLifecycleState
	cleanupMu      sync.Mutex
	cleanup        func()
	closed         bool
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

func (s *Session) startLifecycle(bus *core.EventBus, event core.EventType, ctx *core.EventContext) bool {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.lifecycleState != sessionLifecycleInitial {
		return false
	}
	s.lifecycleState = sessionLifecycleStarted
	bus.EmitAsync(event, ctx)
	return true
}

func (s *Session) stopLifecycle(bus *core.EventBus, event core.EventType, ctx *core.EventContext) bool {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	switch s.lifecycleState {
	case sessionLifecycleInitial:
		s.lifecycleState = sessionLifecycleStopped
		return false
	case sessionLifecycleStarted:
		s.lifecycleState = sessionLifecycleStopped
		bus.EmitAsync(event, ctx)
		return true
	default:
		return false
	}
}

func (s *Session) setCleanup(cleanup func()) {
	s.cleanupMu.Lock()
	if !s.closed {
		s.cleanup = cleanup
		s.cleanupMu.Unlock()
		return
	}
	s.cleanupMu.Unlock()
	if cleanup != nil {
		cleanup()
	}
}

func (s *Session) isClosed() bool {
	s.cleanupMu.Lock()
	defer s.cleanupMu.Unlock()
	return s.closed
}

// Close shuts down the session, closing the PeerConnection and cleaning up resources.
func (s *Session) Close() {
	s.closeOnce.Do(func() {
		if s.done != nil {
			close(s.done)
		}
		s.cleanupMu.Lock()
		s.closed = true
		cleanup := s.cleanup
		s.cleanup = nil
		s.cleanupMu.Unlock()
		if cleanup != nil {
			cleanup()
		}
		if s.pc != nil {
			_ = s.pc.Close()
		}
		if s.module != nil {
			s.module.removeSession(s)
		}
		slog.Info("session closed", "module", "webrtc", "session", s.id, "role", s.role, "stream", s.streamKey)
	})
}
