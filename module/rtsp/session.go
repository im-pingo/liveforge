package rtsp

import (
	"fmt"
	"log/slog"
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
	UDP       *UDPTransport       // non-nil for UDP unicast
	Multicast *MulticastTransport // non-nil for UDP multicast
}

// RTSPSession represents an RTSP session with state management.
type RTSPSession struct {
	ID         string
	StreamKey  string
	RemoteAddr string
	State      SessionState
	Timeout    time.Duration

	Publisher  *RTSPPublisher
	Subscriber *RTSPSubscriber
	MediaInfo  *avframe.MediaInfo
	Tracks     []TrackSetup
	Stream     *core.Stream

	lastTouch  time.Time
	mu         sync.Mutex
	closed     bool
	published  bool
	subscribed bool
}

// MarkPublished records that the async publish lifecycle started.
func (s *RTSPSession) MarkPublished() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.published = true
	return true
}

func (s *RTSPSession) MarkSubscribed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.subscribed = true
	return true
}

func (s *RTSPSession) lifecycleStarted() (published, subscribed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.published, s.subscribed
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

func (s *RTSPSession) GetState() SessionState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.State
}

// IsExpired returns true if the session has exceeded its timeout.
func (s *RTSPSession) IsExpired() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Since(s.lastTouch) > s.Timeout
}

// Close cleans up publisher, subscriber, and UDP transport resources. The
// first caller owns module-level cleanup and receives true.
func (s *RTSPSession) Close() bool {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return false
	}
	s.closed = true
	s.State = StateClosed
	publisher := s.Publisher
	subscriber := s.Subscriber
	stream := s.Stream
	tracks := append([]TrackSetup(nil), s.Tracks...)
	s.mu.Unlock()

	if publisher != nil {
		if err := publisher.Close(); err != nil {
			slog.Error("error closing publisher", "module", "rtsp", "session", s.ID, "error", err)
		}
		if stream != nil {
			stream.RemovePublisherIf(publisher)
		}
	}
	if subscriber != nil {
		if err := subscriber.Close(); err != nil {
			slog.Error("error closing subscriber", "module", "rtsp", "session", s.ID, "error", err)
		}
	}
	for i := range tracks {
		if tracks[i].UDP != nil {
			tracks[i].UDP.Close()
		}
	}
	return true
}
