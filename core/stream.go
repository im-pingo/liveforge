package core

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/util"
)

// StreamState represents the lifecycle state of a stream.
type StreamState uint8

const (
	StreamStateIdle        StreamState = iota + 1
	StreamStateWaitingPull
	StreamStatePublishing
	StreamStateNoPublisher
	StreamStateDestroying
)

func (s StreamState) String() string {
	switch s {
	case StreamStateIdle:
		return "idle"
	case StreamStateWaitingPull:
		return "waiting_pull"
	case StreamStatePublishing:
		return "publishing"
	case StreamStateNoPublisher:
		return "no_publisher"
	case StreamStateDestroying:
		return "destroying"
	default:
		return "unknown"
	}
}

// SkipTracker tracks ring buffer skip events and determines when a subscriber
// has exceeded the allowed skip threshold within a sliding time window.
type SkipTracker struct {
	maxCount int
	window   time.Duration
	events   []time.Time
}

// NewSkipTracker creates a new SkipTracker. If maxCount <= 0, tracking is disabled.
func NewSkipTracker(maxCount int, window time.Duration) *SkipTracker {
	return &SkipTracker{
		maxCount: maxCount,
		window:   window,
	}
}

// RecordSkip records a skip event and returns true if the threshold has been exceeded.
// Always returns false when tracking is disabled (maxCount <= 0).
func (st *SkipTracker) RecordSkip() bool {
	if st.maxCount <= 0 {
		return false
	}

	now := time.Now()

	// Trim events outside the window
	cutoff := now.Add(-st.window)
	trimIdx := 0
	for trimIdx < len(st.events) && st.events[trimIdx].Before(cutoff) {
		trimIdx++
	}
	if trimIdx > 0 {
		st.events = st.events[trimIdx:]
	}

	st.events = append(st.events, now)
	return len(st.events) > st.maxCount
}

// Stream manages the lifecycle, publisher, subscribers, and frame distribution for a stream key.
type Stream struct {
	key    string
	config config.StreamConfig
	limits config.LimitsConfig

	mu        sync.RWMutex
	state     StreamState
	publisher Publisher

	ringBuffer   *util.RingBuffer[*avframe.AVFrame]
	muxerManager *MuxerManager
	gopCache     [][]*avframe.AVFrame
	audioCache   []*avframe.AVFrame
	subscribers  map[string]int // protocol -> count (e.g. "rtmp" -> 2)

	videoSeqHeader *avframe.AVFrame
	audioSeqHeader *avframe.AVFrame

	stats            StreamStats
	eventBus         *EventBus
	noPublisherTimer *time.Timer
	idleTimer        *time.Timer
}

// NewStream creates a new Stream in idle state.
func NewStream(key string, cfg config.StreamConfig, limits config.LimitsConfig, bus *EventBus) *Stream {
	s := &Stream{
		key:         key,
		config:      cfg,
		limits:      limits,
		state:       StreamStateIdle,
		ringBuffer:  util.NewRingBuffer[*avframe.AVFrame](cfg.RingBufferSize),
		eventBus:    bus,
		subscribers: make(map[string]int),
	}
	s.muxerManager = NewMuxerManager(s, cfg.RingBufferSize)
	return s
}

// Key returns the stream key.
func (s *Stream) Key() string {
	return s.key
}

// Config returns the stream configuration.
func (s *Stream) Config() config.StreamConfig {
	return s.config
}

// State returns the current stream state.
func (s *Stream) State() StreamState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

// SetPublisher assigns a publisher to this stream.
func (s *Stream) SetPublisher(pub Publisher) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.state == StreamStatePublishing {
		return errors.New("stream already has a publisher")
	}

	// Cancel no-publisher timer if republishing
	if s.noPublisherTimer != nil {
		s.noPublisherTimer.Stop()
		s.noPublisherTimer = nil
	}

	// Cancel idle timer — we have a publisher now
	if s.idleTimer != nil {
		s.idleTimer.Stop()
		s.idleTimer = nil
	}

	s.publisher = pub
	s.state = StreamStatePublishing
	s.stats.initStats()

	return nil
}

// RemovePublisher detaches the publisher and starts the no-publisher timeout.
func (s *Stream) RemovePublisher() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.publisher = nil
	s.state = StreamStateNoPublisher

	if s.config.NoPublisherTimeout > 0 {
		s.noPublisherTimer = time.AfterFunc(s.config.NoPublisherTimeout, func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.state == StreamStateNoPublisher {
				s.state = StreamStateDestroying
			}
		})
	}

	s.checkIdleTimeout()
}

// Close force-closes the stream: closes the ring buffer, removes the publisher,
// and transitions to destroying state.
func (s *Stream) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.state == StreamStateDestroying {
		return
	}

	if s.noPublisherTimer != nil {
		s.noPublisherTimer.Stop()
		s.noPublisherTimer = nil
	}

	if s.idleTimer != nil {
		s.idleTimer.Stop()
		s.idleTimer = nil
	}

	if s.publisher != nil {
		s.publisher.Close() //nolint:errcheck
		s.publisher = nil
	}

	s.state = StreamStateDestroying
	s.ringBuffer.Close()
}

// Publisher returns the current publisher, if any.
func (s *Stream) Publisher() Publisher {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.publisher
}

// WriteFrame writes a media frame to the ring buffer and updates caches.
// Returns false if the frame was rejected due to bitrate limit.
func (s *Stream) WriteFrame(frame *avframe.AVFrame) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Enforce max_bitrate_per_stream: reject non-header frames when over limit
	if maxKbps := s.limits.MaxBitratePerStream; maxKbps > 0 {
		if frame.FrameType != avframe.FrameTypeSequenceHeader {
			snap := s.stats.snapshot()
			if snap.BitrateKbps > int64(maxKbps) {
				return false
			}
		}
	}

	// Store sequence headers separately for late-joining subscribers
	if frame.FrameType == avframe.FrameTypeSequenceHeader {
		if frame.MediaType.IsVideo() {
			s.videoSeqHeader = frame
		} else if frame.MediaType.IsAudio() {
			s.audioSeqHeader = frame
		}
	}

	// Update GOP cache for video frames
	if s.config.GOPCache {
		if frame.MediaType.IsVideo() {
			if frame.FrameType.IsKeyframe() {
				// Start new GOP
				s.gopCache = append(s.gopCache, []*avframe.AVFrame{frame})
				if len(s.gopCache) > s.config.GOPCacheNum {
					s.gopCache = s.gopCache[len(s.gopCache)-s.config.GOPCacheNum:]
				}
			} else if frame.FrameType != avframe.FrameTypeSequenceHeader && len(s.gopCache) > 0 {
				s.gopCache[len(s.gopCache)-1] = append(s.gopCache[len(s.gopCache)-1], frame)
			}
		} else if frame.MediaType.IsAudio() && frame.FrameType != avframe.FrameTypeSequenceHeader && len(s.gopCache) > 0 {
			// Interleave audio into GOP cache for DTS ordering
			s.gopCache[len(s.gopCache)-1] = append(s.gopCache[len(s.gopCache)-1], frame)
		}
	}

	// Update audio cache for late-joining subscribers
	if frame.MediaType.IsAudio() && frame.FrameType != avframe.FrameTypeSequenceHeader && s.config.AudioCacheMs > 0 {
		s.audioCache = append(s.audioCache, frame)
		minDTS := frame.DTS - int64(s.config.AudioCacheMs)
		trimIdx := 0
		for trimIdx < len(s.audioCache) && s.audioCache[trimIdx].DTS < minDTS {
			trimIdx++
		}
		if trimIdx > 0 {
			s.audioCache = s.audioCache[trimIdx:]
		}
	}

	s.stats.recordFrame(len(frame.Payload), frame.MediaType.IsVideo())
	s.ringBuffer.Write(frame)
	return true
}

// GOPCacheLen returns the total number of frames across all cached GOPs.
func (s *Stream) GOPCacheLen() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	total := 0
	for _, gop := range s.gopCache {
		total += len(gop)
	}
	return total
}

// GOPCacheDetail returns GOP cache statistics without copying the frames.
type GOPCacheDetail struct {
	TotalFrames int
	VideoFrames int
	AudioFrames int
	DurationMs  int64
}

func (s *Stream) GOPCacheDetail() GOPCacheDetail {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var d GOPCacheDetail
	for _, gop := range s.gopCache {
		d.TotalFrames += len(gop)
	}
	if d.TotalFrames == 0 {
		return d
	}

	var firstDTS, lastDTS int64
	firstSet := false
	for _, gop := range s.gopCache {
		for _, f := range gop {
			if f.MediaType.IsVideo() {
				d.VideoFrames++
			} else if f.MediaType.IsAudio() {
				d.AudioFrames++
			}
			if f.DTS > 0 {
				if !firstSet {
					firstDTS = f.DTS
					firstSet = true
				}
				lastDTS = f.DTS
			}
		}
	}
	if firstSet {
		d.DurationMs = lastDTS - firstDTS
	}
	return d
}

// GOPCache returns a flattened copy of all cached GOPs.
func (s *Stream) GOPCache() []*avframe.AVFrame {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []*avframe.AVFrame
	for _, gop := range s.gopCache {
		result = append(result, gop...)
	}
	return result
}

// AudioCache returns a copy of the current audio cache.
func (s *Stream) AudioCache() []*avframe.AVFrame {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]*avframe.AVFrame, len(s.audioCache))
	copy(result, s.audioCache)
	return result
}

// VideoSeqHeader returns the cached video sequence header (SPS/PPS), if any.
func (s *Stream) VideoSeqHeader() *avframe.AVFrame {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.videoSeqHeader
}

// AudioSeqHeader returns the cached audio sequence header (AudioSpecificConfig), if any.
func (s *Stream) AudioSeqHeader() *avframe.AVFrame {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.audioSeqHeader
}

// Stats returns a point-in-time snapshot of stream statistics.
func (s *Stream) Stats() StreamStatsSnapshot {
	return s.stats.snapshot()
}

// RingBuffer returns the stream's ring buffer for reader creation.
func (s *Stream) RingBuffer() *util.RingBuffer[*avframe.AVFrame] {
	return s.ringBuffer
}

// MuxerManager returns the stream's muxer manager.
func (s *Stream) MuxerManager() *MuxerManager {
	return s.muxerManager
}

// AddSubscriber increments the subscriber count for a protocol (e.g. "rtmp").
// Returns an error if max_subscribers_per_stream limit is reached.
func (s *Stream) AddSubscriber(protocol string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if max := s.limits.MaxSubscribersPerStream; max > 0 {
		total := 0
		for _, n := range s.subscribers {
			total += n
		}
		if total >= max {
			return fmt.Errorf("max subscribers per stream limit reached (%d)", max)
		}
	}

	s.subscribers[protocol]++

	// Cancel idle timer — we have a subscriber now
	if s.idleTimer != nil {
		s.idleTimer.Stop()
		s.idleTimer = nil
	}

	return nil
}

// RemoveSubscriber decrements the subscriber count for a protocol.
func (s *Stream) RemoveSubscriber(protocol string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subscribers[protocol]--
	if s.subscribers[protocol] <= 0 {
		delete(s.subscribers, protocol)
	}
	s.checkIdleTimeout()
}

// Subscribers returns a snapshot of protocol-level subscriber counts.
func (s *Stream) Subscribers() map[string]int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make(map[string]int, len(s.subscribers))
	for k, v := range s.subscribers {
		result[k] = v
	}
	return result
}

// totalSubscribers returns the sum of all subscriber counts. Must hold mu.
func (s *Stream) totalSubscribers() int {
	total := 0
	for _, n := range s.subscribers {
		total += n
	}
	return total
}

// checkIdleTimeout starts or cancels the idle timer based on current state.
// Must hold mu.
func (s *Stream) checkIdleTimeout() {
	if s.config.IdleTimeout <= 0 {
		return
	}
	if s.publisher == nil && s.totalSubscribers() == 0 {
		if s.idleTimer == nil {
			s.idleTimer = time.AfterFunc(s.config.IdleTimeout, func() {
				s.mu.Lock()
				defer s.mu.Unlock()
				if s.publisher == nil && s.totalSubscribers() == 0 {
					s.state = StreamStateDestroying
				}
			})
		}
	} else {
		if s.idleTimer != nil {
			s.idleTimer.Stop()
			s.idleTimer = nil
		}
	}
}
