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
	sendGate  *whepSendGate

	lifecycleMu    sync.Mutex
	lifecycleState sessionLifecycleState
	cleanupMu      sync.Mutex
	cleanup        func()
	closed         bool
	feedStatusMu   sync.RWMutex
	feedStatus     *whepFeedStatus
	feedVideo      *TrackSender
	feedAudio      *TrackSender
	feedRTPStats   *rtpPeerStats
}

func (s *Session) setFeedStatus(status *whepFeedStatus) {
	s.feedStatusMu.Lock()
	s.feedStatus = status
	s.feedStatusMu.Unlock()
}

func (s *Session) setFeedTracks(video, audio *TrackSender, transportStats *rtpPeerStats) {
	s.feedStatusMu.Lock()
	s.feedVideo = video
	s.feedAudio = audio
	s.feedRTPStats = transportStats
	s.feedStatusMu.Unlock()
}

func (s *Session) FeedStatus() (WHEPFeedStatus, bool) {
	s.refreshFeedTransportStats()
	s.feedStatusMu.RLock()
	status := s.feedStatus
	s.feedStatusMu.RUnlock()
	if status == nil {
		return WHEPFeedStatus{}, false
	}
	return status.Snapshot(), true
}

func (s *Session) refreshFeedTransportStats() {
	s.captureFeedTransportStats(false)
}

func (s *Session) finalizeFeedTransportStats() {
	s.captureFeedTransportStats(true)
}

func (s *Session) captureFeedTransportStats(final bool) {
	s.feedStatusMu.RLock()
	status := s.feedStatus
	video := s.feedVideo
	audio := s.feedAudio
	transportStats := s.feedRTPStats
	s.feedStatusMu.RUnlock()
	if status == nil {
		return
	}

	var rtpPackets, rtpBytes uint64
	var rtcpPackets uint64
	if video != nil {
		packets, bytes := trackRTPTransportStats(video, transportStats)
		rtpPackets += packets
		rtpBytes += bytes
		rtcpPackets += video.Stats.RTCPPacketsReceived.Load()
	}
	if audio != nil {
		packets, bytes := trackRTPTransportStats(audio, transportStats)
		rtpPackets += packets
		rtpBytes += bytes
		rtcpPackets += audio.Stats.RTCPPacketsReceived.Load()
	}
	if final {
		status.setFinalTransportStats(rtpPackets, rtpBytes, rtcpPackets)
		return
	}
	status.SetTransportStats(rtpPackets, rtpBytes, rtcpPackets)
}

func trackRTPTransportStats(track *TrackSender, transportStats *rtpPeerStats) (packets, bytes uint64) {
	if track == nil || transportStats == nil || track.sender == nil {
		return 0, 0
	}
	for _, encoding := range track.sender.GetParameters().Encodings {
		count, size := transportStats.snapshot(uint32(encoding.SSRC))
		packets += count
		bytes += size
		if encoding.RTX.SSRC != 0 {
			count, size = transportStats.snapshot(uint32(encoding.RTX.SSRC))
			packets += count
			bytes += size
		}
		if encoding.FEC.SSRC != 0 {
			count, size = transportStats.snapshot(uint32(encoding.FEC.SSRC))
			packets += count
			bytes += size
		}
	}
	return packets, bytes
}

func (s *Session) statusResponse() (sessionStatusResponse, bool) {
	feed, ok := s.FeedStatus()
	if !ok {
		return sessionStatusResponse{}, false
	}
	return sessionStatusResponse{
		SessionID: s.id,
		StreamKey: s.streamKey,
		Role:      s.role,
		Feed:      feed,
	}, true
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
		sendGate:  newWHEPSendGate(),
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
	if err := bus.EmitAsync(event, ctx); err != nil {
		return false
	}
	s.lifecycleState = sessionLifecycleStarted
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
		if err := bus.EmitAsync(event, ctx); err != nil {
			slog.Error("failed to enqueue WebRTC terminal lifecycle event", "event", event, "stream", ctx.StreamKey, "error", err)
			return false
		}
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
		s.sendGate.close()
		if s.done != nil {
			close(s.done)
		}
		s.finalizeFeedTransportStats()
		s.feedStatusMu.RLock()
		feedStatus := s.feedStatus
		s.feedStatusMu.RUnlock()
		if feedStatus != nil {
			feedStatus.SetState(WHEPFeedClosed)
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
