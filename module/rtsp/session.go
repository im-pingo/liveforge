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

type trackSetupResult int

const (
	trackSetupOK trackSetupResult = iota
	trackSetupSessionClosed
	trackSetupInvalidState
)

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

type RTSPSessionSnapshot struct {
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
	Closed     bool
	Published  bool
	Subscribed bool
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

func (s *RTSPSession) startPublishLifecycle(emit func()) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.State == StateClosed || s.Publisher == nil {
		return false
	}
	s.published = true
	emit()
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

func (s *RTSPSession) startSubscribeLifecycle(emit func()) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.State == StateClosed || s.Subscriber == nil {
		return false
	}
	s.subscribed = true
	emit()
	return true
}

func (s *RTSPSession) lifecycleStarted() (published, subscribed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.published, s.subscribed
}

func (s *RTSPSession) CanHandleRequest() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.closed && s.State != StateClosed
}

func (s *RTSPSession) SetRemoteAddr(remoteAddr string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.State == StateClosed {
		return false
	}
	s.RemoteAddr = remoteAddr
	return true
}

func (s *RTSPSession) SetDescription(mediaInfo *avframe.MediaInfo, stream *core.Stream) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.State == StateClosed {
		return false
	}
	s.MediaInfo = mediaInfo
	s.Stream = stream
	return true
}

func (s *RTSPSession) setupTrack(track TrackSetup) trackSetupResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	if result := s.validateSetupTrackLocked(track.TrackID); result != trackSetupOK {
		return result
	}
	if s.State == StateDescribed || s.State == StateAnnounced {
		s.State = StateReady
	}
	s.lastTouch = time.Now()
	track.Codec = s.codecForTrackLocked(track.TrackID)
	s.Tracks = append(s.Tracks, track)
	return trackSetupOK
}

func (s *RTSPSession) validateSetupTrack(trackID int) trackSetupResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.validateSetupTrackLocked(trackID)
}

func (s *RTSPSession) validateSetupTrackLocked(trackID int) trackSetupResult {
	if s.closed || s.State == StateClosed {
		return trackSetupSessionClosed
	}
	switch s.State {
	case StateDescribed, StateAnnounced, StateReady:
	default:
		return trackSetupInvalidState
	}
	if trackID < 0 || s.MediaInfo == nil || trackID >= s.mediaTrackCountLocked() {
		return trackSetupInvalidState
	}
	for _, track := range s.Tracks {
		if track.TrackID == trackID {
			return trackSetupInvalidState
		}
	}
	return trackSetupOK
}

func (s *RTSPSession) mediaTrackCountLocked() int {
	count := 0
	if s.MediaInfo.HasVideo() {
		count++
	}
	if s.MediaInfo.HasAudio() {
		count++
	}
	return count
}

func (s *RTSPSession) codecForTrackLocked(trackID int) avframe.CodecType {
	if s.MediaInfo.HasVideo() {
		if trackID == 0 {
			return s.MediaInfo.VideoCodec
		}
		return s.MediaInfo.AudioCodec
	}
	return s.MediaInfo.AudioCodec
}

func (s *RTSPSession) SetPublisher(mediaInfo *avframe.MediaInfo, stream *core.Stream, publisher *RTSPPublisher) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.State == StateClosed {
		return false
	}
	s.MediaInfo = mediaInfo
	s.Stream = stream
	s.Publisher = publisher
	return true
}

func (s *RTSPSession) ClearPublisher(expected *RTSPPublisher) {
	s.mu.Lock()
	if s.Publisher == expected {
		s.Publisher = nil
	}
	s.mu.Unlock()
}

func (s *RTSPSession) SetSubscriber(subscriber *RTSPSubscriber) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.State == StateClosed || s.Stream == nil {
		return false
	}
	s.Subscriber = subscriber
	return true
}

func (s *RTSPSession) ClearSubscriber(expected *RTSPSubscriber) {
	s.mu.Lock()
	if s.Subscriber == expected {
		s.Subscriber = nil
	}
	s.mu.Unlock()
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
	if s.closed {
		return fmt.Errorf("session closed")
	}
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

func (s *RTSPSession) touchPublisherIfCurrent(stream *core.Stream, publisher *RTSPPublisher) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.State == StateClosed || s.Stream != stream || s.Publisher != publisher {
		return false
	}
	s.lastTouch = time.Now()
	return true
}

func (s *RTSPSession) GetState() SessionState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.State
}

func (s *RTSPSession) Snapshot() RTSPSessionSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshotLocked()
}

func (s *RTSPSession) snapshotLocked() RTSPSessionSnapshot {
	return RTSPSessionSnapshot{
		ID:         s.ID,
		StreamKey:  s.StreamKey,
		RemoteAddr: s.RemoteAddr,
		State:      s.State,
		Timeout:    s.Timeout,
		Publisher:  s.Publisher,
		Subscriber: s.Subscriber,
		MediaInfo:  s.MediaInfo,
		Tracks:     append([]TrackSetup(nil), s.Tracks...),
		Stream:     s.Stream,
		Closed:     s.closed,
		Published:  s.published,
		Subscribed: s.subscribed,
	}
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
	_, closed := s.closeSnapshot()
	return closed
}

func (s *RTSPSession) closeSnapshot() (RTSPSessionSnapshot, bool) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return RTSPSessionSnapshot{}, false
	}
	s.closed = true
	s.State = StateClosed
	snapshot := s.snapshotLocked()
	s.mu.Unlock()

	if snapshot.Publisher != nil {
		if err := snapshot.Publisher.Close(); err != nil {
			slog.Error("error closing publisher", "module", "rtsp", "session", snapshot.ID, "error", err)
		}
		if snapshot.Stream != nil {
			snapshot.Stream.RemovePublisherIf(snapshot.Publisher)
		}
	}
	if snapshot.Subscriber != nil {
		if err := snapshot.Subscriber.Close(); err != nil {
			slog.Error("error closing subscriber", "module", "rtsp", "session", snapshot.ID, "error", err)
		}
	}
	for i := range snapshot.Tracks {
		if snapshot.Tracks[i].UDP != nil {
			snapshot.Tracks[i].UDP.Close()
		}
		if snapshot.Tracks[i].Multicast != nil {
			snapshot.Tracks[i].Multicast.Close()
		}
	}
	return snapshot, true
}
