package rtsp

import (
	"fmt"
	"sync"
	"time"
)

// SessionState represents the RTSP session state.
type SessionState int

const (
	StateInit SessionState = iota
	StateDescribed
	StateAnnounced
	StateReady
	StatePlaying
	StateRecording
	StateClosed
)

// allowedTransitions defines valid state transitions.
var allowedTransitions = map[SessionState][]SessionState{
	StateInit:      {StateDescribed, StateAnnounced, StateClosed},
	StateDescribed: {StateReady, StateClosed},
	StateAnnounced: {StateReady, StateClosed},
	StateReady:     {StatePlaying, StateRecording, StateClosed},
	StatePlaying:   {StateReady, StateClosed},
	StateRecording: {StateClosed},
}

const DefaultTimeout = 60 * time.Second

// RTSPSession represents an RTSP session with state management.
type RTSPSession struct {
	ID        string
	StreamKey string
	State     SessionState
	Timeout   time.Duration
	lastTouch time.Time
	mu        sync.Mutex
}

func NewRTSPSession(id, streamKey string) *RTSPSession {
	return &RTSPSession{
		ID:        id,
		StreamKey: streamKey,
		State:     StateInit,
		Timeout:   DefaultTimeout,
		lastTouch: time.Now(),
	}
}

// Transition moves the session to a new state if the transition is valid.
func (s *RTSPSession) Transition(newState SessionState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	allowed, ok := allowedTransitions[s.State]
	if !ok {
		return fmt.Errorf("no transitions from state %d", s.State)
	}
	for _, a := range allowed {
		if a == newState {
			s.State = newState
			s.lastTouch = time.Now()
			return nil
		}
	}
	return fmt.Errorf("invalid transition from %d to %d", s.State, newState)
}

// Touch resets the session timeout timer.
func (s *RTSPSession) Touch() {
	s.mu.Lock()
	s.lastTouch = time.Now()
	s.mu.Unlock()
}

// IsExpired returns true if the session has exceeded its timeout.
func (s *RTSPSession) IsExpired() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Since(s.lastTouch) > s.Timeout
}
