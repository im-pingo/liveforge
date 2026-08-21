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
	TrackID     int
	Control     string
	PayloadType uint8
	Codec       avframe.CodecType
	Transport   TransportConfig
	UDP         *UDPTransport       // non-nil for UDP unicast
	Multicast   *MulticastTransport // non-nil for UDP multicast
}

// RTSPSession represents an RTSP session with state management.
type RTSPSession struct {
	ID        string
	StreamKey string
	State     SessionState
	Timeout   time.Duration

	Publisher           *RTSPPublisher
	PublisherGeneration uint64
	Subscriber          *RTSPSubscriber
	MediaInfo           *avframe.MediaInfo
	Tracks              []TrackSetup
	TrackDescriptions   []RTPTrackDescription
	Stream              *core.Stream

	lastTouch time.Time
	mu        sync.Mutex
	tracksMu  sync.RWMutex
	closeOnce sync.Once
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

func (s *RTSPSession) stateSnapshot() SessionState {
	s.mu.Lock()
	state := s.State
	s.mu.Unlock()
	return state
}

func (s *RTSPSession) publisherSnapshot() (*RTSPPublisher, *core.Stream, uint64) {
	s.mu.Lock()
	pub, stream, generation := s.Publisher, s.Stream, s.PublisherGeneration
	s.mu.Unlock()
	return pub, stream, generation
}

func (s *RTSPSession) subscriberSnapshot() *RTSPSubscriber {
	s.mu.Lock()
	subscriber := s.Subscriber
	s.mu.Unlock()
	return subscriber
}

func (s *RTSPSession) streamMediaSnapshot() (*core.Stream, *avframe.MediaInfo) {
	s.mu.Lock()
	stream, mediaInfo := s.Stream, s.MediaInfo
	s.mu.Unlock()
	return stream, mediaInfo
}

func (s *RTSPSession) setDescription(stream *core.Stream, mediaInfo *avframe.MediaInfo, descriptions []RTPTrackDescription) {
	s.mu.Lock()
	s.Stream = stream
	s.MediaInfo = mediaInfo
	s.TrackDescriptions = descriptions
	s.mu.Unlock()
}

func (s *RTSPSession) setPublishState(stream *core.Stream, publisher *RTSPPublisher, generation uint64) {
	s.mu.Lock()
	s.Stream = stream
	s.Publisher = publisher
	s.PublisherGeneration = generation
	s.mu.Unlock()
}

func (s *RTSPSession) setSubscriber(subscriber *RTSPSubscriber) {
	s.mu.Lock()
	s.Subscriber = subscriber
	s.mu.Unlock()
}

func (s *RTSPSession) detachPublisher() (*RTSPPublisher, *core.Stream, uint64) {
	s.mu.Lock()
	pub, stream, generation := s.Publisher, s.Stream, s.PublisherGeneration
	s.Publisher = nil
	s.PublisherGeneration = 0
	s.mu.Unlock()
	return pub, stream, generation
}

func (s *RTSPSession) detachSubscriber() (*RTSPSubscriber, *core.Stream) {
	s.mu.Lock()
	subscriber, stream := s.Subscriber, s.Stream
	s.Subscriber = nil
	s.mu.Unlock()
	return subscriber, stream
}

// Close cleans up publisher, subscriber, and UDP transport resources.
func (s *RTSPSession) Close() {
	s.closeOnce.Do(func() {
		if publisher, stream, generation := s.detachPublisher(); publisher != nil {
			if err := publisher.Close(); err != nil {
				slog.Error("error closing publisher", "module", "rtsp", "session", s.ID, "error", err)
			}
			if stream != nil {
				stream.RemovePublisherIfGeneration(generation)
			}
		}
		if subscriber, stream := s.detachSubscriber(); subscriber != nil {
			if err := subscriber.Close(); err != nil {
				slog.Error("error closing subscriber", "module", "rtsp", "session", s.ID, "error", err)
			}
			if stream != nil {
				stream.RemoveSubscriber("rtsp")
			}
		}
		s.closeTracks()
	})
}

func (s *RTSPSession) addTrack(track TrackSetup) {
	s.tracksMu.Lock()
	s.Tracks = append(s.Tracks, track)
	s.tracksMu.Unlock()
}

func (s *RTSPSession) trackSnapshot() []TrackSetup {
	s.tracksMu.RLock()
	tracks := append([]TrackSetup(nil), s.Tracks...)
	s.tracksMu.RUnlock()
	return tracks
}

func (s *RTSPSession) closeTracks() {
	s.tracksMu.Lock()
	udp := make([]*UDPTransport, 0, len(s.Tracks))
	multicast := make([]*MulticastTransport, 0, len(s.Tracks))
	for i := range s.Tracks {
		if s.Tracks[i].UDP != nil {
			udp = append(udp, s.Tracks[i].UDP)
			s.Tracks[i].UDP = nil
		}
		if s.Tracks[i].Multicast != nil {
			multicast = append(multicast, s.Tracks[i].Multicast)
			s.Tracks[i].Multicast = nil
		}
	}
	s.tracksMu.Unlock()

	for _, transport := range udp {
		transport.Close()
	}
	for _, transport := range multicast {
		transport.Close()
	}
}
