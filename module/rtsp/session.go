package rtsp

import (
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
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

// TrackSetup holds the transport configuration for a single track.
type TrackSetup struct {
	TrackID   int
	Codec     avframe.CodecType
	Transport TransportConfig
	UDP       *UDPTransport // non-nil for UDP transport
}

// RTSPSession represents an RTSP session with state management.
type RTSPSession struct {
	ID        string
	StreamKey string
	State     SessionState
	Timeout   time.Duration

	Publisher  *RTSPPublisher
	Subscriber *RTSPSubscriber
	MediaInfo  *avframe.MediaInfo
	Tracks     []TrackSetup
	Stream     *core.Stream

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

// Close cleans up publisher, subscriber, and UDP transport resources.
func (s *RTSPSession) Close() {
	if s.Publisher != nil {
		if err := s.Publisher.Close(); err != nil {
			log.Printf("rtsp: error closing publisher for session %s: %v", s.ID, err)
		}
		if s.Stream != nil {
			s.Stream.RemovePublisher()
		}
		s.Publisher = nil
	}
	if s.Subscriber != nil {
		if err := s.Subscriber.Close(); err != nil {
			log.Printf("rtsp: error closing subscriber for session %s: %v", s.ID, err)
		}
		if s.Stream != nil {
			s.Stream.RemoveSubscriber("rtsp")
		}
		s.Subscriber = nil
	}
	// Close any UDP transports.
	for i := range s.Tracks {
		if s.Tracks[i].UDP != nil {
			s.Tracks[i].UDP.Close()
			s.Tracks[i].UDP = nil
		}
	}
}
