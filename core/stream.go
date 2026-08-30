package core

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/util"
)

// StreamState represents the lifecycle state of a stream.
type StreamState uint8

const (
	StreamStateIdle StreamState = iota + 1
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
	key        string
	instanceID uint64
	config     config.StreamConfig
	limits     config.LimitsConfig

	mu                      sync.RWMutex
	state                   StreamState
	publisher               Publisher
	usedPublisherIDs        map[string]struct{}
	lastPublisherID         string
	lastPublisherGeneration uint64

	ringBuffer            *util.RingBuffer[*avframe.AVFrame]
	muxerManager          *MuxerManager
	gopCache              [][]*avframe.AVFrame
	gopStarts             []int64
	gopBytes              []int64
	gopMinDTS             []int64
	gopMaxDTS             []int64
	gopGeneration         uint64
	gopCacheSealed        bool
	subscribers           map[string]int // protocol -> count (e.g. "rtmp" -> 2)
	generationSubscribers map[uint64]map[string]int

	videoSeqHeader *avframe.AVFrame
	audioSeqHeader *avframe.AVFrame
	seqHeaderReady chan struct{} // closed when first sequence header arrives

	publisherGeneration   uint64
	generationStartCursor int64
	mediaInfo             avframe.MediaInfo
	audioCodecEpoch       uint64
	generationDone        chan struct{}
	generationBoundary    *streamGenerationBoundary
	startupStateChanged   chan struct{}
	startupReady          bool

	stats            StreamStats
	eventBus         *EventBus
	noPublisherTimer *time.Timer
	idleTimer        *time.Timer
	feedbackRouter   *FeedbackRouter
	transcodeManager *TranscodeManager
	destroyCallback  func()
	destroyOnce      sync.Once
}

var streamInstanceSequence atomic.Uint64

// streamGenerationBoundary retains the immutable end position of one
// publisher generation after that publisher detaches. Snapshots keep this
// object alive without retaining the Stream or its publisher.
type streamGenerationBoundary struct {
	endCursor atomic.Int64
	ended     atomic.Bool
}

func (b *streamGenerationBoundary) end() (int64, bool) {
	if b == nil || !b.ended.Load() {
		return 0, false
	}
	return b.endCursor.Load(), true
}

func normalizeGOPConfig(cfg config.StreamConfig) config.StreamConfig {
	if cfg.GOPCache && cfg.GOPCacheNum > 0 && cfg.GOPCacheMaxFrames <= 0 && cfg.GOPCacheMaxBytes <= 0 {
		cfg.GOPCacheMaxFrames = config.DefaultGOPCacheMaxFrames
	}
	return cfg
}

// NewStream creates a new Stream in idle state.
func NewStream(key string, cfg config.StreamConfig, limits config.LimitsConfig, bus *EventBus) *Stream {
	cfg = normalizeGOPConfig(cfg)
	s := &Stream{
		key:                   key,
		instanceID:            streamInstanceSequence.Add(1),
		config:                cfg,
		limits:                limits,
		state:                 StreamStateIdle,
		ringBuffer:            util.NewRingBuffer[*avframe.AVFrame](cfg.RingBufferSize),
		eventBus:              bus,
		subscribers:           make(map[string]int),
		usedPublisherIDs:      make(map[string]struct{}),
		generationSubscribers: make(map[uint64]map[string]int),
		seqHeaderReady:        make(chan struct{}),
		startupStateChanged:   make(chan struct{}),
	}
	s.muxerManager = NewMuxerManager(s, cfg.RingBufferSize)
	s.feedbackRouter = NewFeedbackRouter(cfg.Feedback)
	return s
}

// InstanceID identifies this concrete stream object without retaining the
// object itself in registries or lifecycle events.
func (s *Stream) InstanceID() uint64 { return s.instanceID }

func (s *Stream) setDestroyCallback(callback func()) {
	s.mu.Lock()
	s.destroyCallback = callback
	s.mu.Unlock()
}

func (s *Stream) notifyDestroy() {
	s.destroyOnce.Do(func() {
		s.mu.RLock()
		callback := s.destroyCallback
		s.mu.RUnlock()
		if callback != nil {
			callback()
		}
	})
}

// Key returns the stream key.
func (s *Stream) Key() string {
	return s.key
}

// Config returns the stream configuration.
func (s *Stream) Config() config.StreamConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.config
}

// UpdatePolicy applies runtime-safe stream policy values. The ring buffer and
// muxer capacity are intentionally not resized in place; new streams receive
// those structural values from StreamHub.
func (s *Stream) UpdatePolicy(cfg config.StreamConfig, limits config.LimitsConfig) {
	cfg = normalizeGOPConfig(cfg)
	s.mu.Lock()
	s.config = cfg
	s.limits = limits
	if !cfg.GOPCache || cfg.GOPCacheNum <= 0 {
		s.gopCache = nil
		s.gopStarts = nil
		s.gopBytes = nil
		s.gopMinDTS = nil
		s.gopMaxDTS = nil
		s.gopCacheSealed = false
	} else if len(s.gopCache) > cfg.GOPCacheNum {
		s.gopCache = append([][]*avframe.AVFrame(nil), s.gopCache[len(s.gopCache)-cfg.GOPCacheNum:]...)
		s.gopStarts = append([]int64(nil), s.gopStarts[len(s.gopStarts)-cfg.GOPCacheNum:]...)
		s.gopBytes = append([]int64(nil), s.gopBytes[len(s.gopBytes)-cfg.GOPCacheNum:]...)
		s.gopMinDTS = append([]int64(nil), s.gopMinDTS[len(s.gopMinDTS)-cfg.GOPCacheNum:]...)
		s.gopMaxDTS = append([]int64(nil), s.gopMaxDTS[len(s.gopMaxDTS)-cfg.GOPCacheNum:]...)
	}
	s.trimGOPCacheLocked()
	if s.noPublisherTimer != nil {
		s.noPublisherTimer.Stop()
		s.noPublisherTimer = nil
	}
	if s.state == StreamStateNoPublisher && cfg.NoPublisherTimeout > 0 {
		s.noPublisherTimer = time.AfterFunc(cfg.NoPublisherTimeout, func() {
			s.mu.Lock()
			notify := false
			if s.state == StreamStateNoPublisher {
				s.usedPublisherIDs = nil
				s.state = StreamStateDestroying
				s.signalStartupStateChangedLocked()
				notify = true
			}
			s.mu.Unlock()
			if notify {
				s.notifyDestroy()
			}
		})
	}
	if s.idleTimer != nil {
		s.idleTimer.Stop()
		s.idleTimer = nil
	}
	s.checkIdleTimeout()
	feedback := s.feedbackRouter
	s.mu.Unlock()
	if feedback != nil {
		feedback.UpdateConfig(cfg.Feedback)
	}
}

// State returns the current stream state.
func (s *Stream) State() StreamState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

// SetPublisher assigns a publisher to this stream.
func (s *Stream) SetPublisher(pub Publisher) error {
	if isNilPublisher(pub) {
		return errors.New("publisher is nil")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.state == StreamStatePublishing {
		return errors.New("stream already has a publisher")
	}
	if s.state == StreamStateDestroying {
		return errors.New("stream is destroying")
	}
	publisherID := pub.ID()
	if _, used := s.usedPublisherIDs[publisherID]; publisherID != "" && used {
		return fmt.Errorf("publisher ID %q was already used by an earlier generation", publisherID)
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

	if s.transcodeManager != nil {
		s.transcodeManager.Reset()
	}

	s.closeGenerationLocked()
	s.publisherGeneration++
	s.generationStartCursor = s.ringBuffer.WriteCursor()
	s.gopCache = nil
	s.gopStarts = nil
	s.gopBytes = nil
	s.gopMinDTS = nil
	s.gopMaxDTS = nil
	s.gopGeneration = 0
	s.gopCacheSealed = false
	s.videoSeqHeader = nil
	s.audioSeqHeader = nil
	s.seqHeaderReady = make(chan struct{})
	s.mediaInfo = avframe.MediaInfo{}
	s.audioCodecEpoch = 0
	s.startupReady = false
	s.generationDone = make(chan struct{})
	s.generationBoundary = &streamGenerationBoundary{}
	s.mergePublisherMediaInfoLocked(pub.MediaInfo())
	if s.mediaInfo.AudioCodec != 0 {
		s.audioCodecEpoch = 1
		if s.audioSeqHeader != nil {
			s.audioSeqHeader.AudioCodecEpoch = s.audioCodecEpoch
			s.audioSeqHeader.AudioProvenance = avframe.FrameProvenanceSource
		}
	}
	s.publisher = pub
	s.lastPublisherID = publisherID
	s.lastPublisherGeneration = s.publisherGeneration
	s.state = StreamStatePublishing
	s.startupReady = s.startupReadyLocked()
	s.stats.initStats()
	s.signalStartupStateChangedLocked()
	if publisherID != "" {
		s.usedPublisherIDs[publisherID] = struct{}{}
	}

	return nil
}

// RemovePublisher detaches the publisher and starts the no-publisher timeout.
func (s *Stream) RemovePublisher() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removePublisherLocked()
}

// RemovePublisherIf detaches pub only when it is still the active publisher.
// It prevents a delayed connection cleanup from removing a replacement.
func (s *Stream) RemovePublisherIf(pub Publisher) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.publisherMatchesLocked(pub) {
		return false
	}
	s.removePublisherLocked()
	return true
}

func (s *Stream) removePublisherLocked() {
	if s.state == StreamStateDestroying {
		return
	}
	s.closeGenerationLocked()
	s.publisher = nil
	s.state = StreamStateNoPublisher
	s.startupReady = false
	s.signalStartupStateChangedLocked()

	if s.config.NoPublisherTimeout > 0 {
		s.noPublisherTimer = time.AfterFunc(s.config.NoPublisherTimeout, func() {
			s.mu.Lock()
			notify := false
			if s.state == StreamStateNoPublisher {
				s.usedPublisherIDs = nil
				s.state = StreamStateDestroying
				s.signalStartupStateChangedLocked()
				notify = true
			}
			s.mu.Unlock()
			if notify {
				s.notifyDestroy()
			}
		})
	}

	s.checkIdleTimeout()
}

func samePublisher(left, right Publisher) bool {
	if isNilPublisher(left) || isNilPublisher(right) {
		return false
	}
	leftType := reflect.TypeOf(left)
	if leftType != reflect.TypeOf(right) || !leftType.Comparable() {
		return false
	}
	return left == right
}

// publisherMatchesLocked uses the publisher contract's stable ID on the frame
// hot path. The reflective comparison remains only for legacy publishers that
// do not provide an ID, which are outside the normal protocol adapters.
func (s *Stream) publisherMatchesLocked(pub Publisher) bool {
	if s.state != StreamStatePublishing || isNilPublisher(s.publisher) || isNilPublisher(pub) {
		return false
	}
	candidateID := pub.ID()
	if s.lastPublisherID != "" || candidateID != "" {
		return s.lastPublisherID != "" && s.lastPublisherID == candidateID
	}
	return samePublisher(s.publisher, pub)
}

func isNilPublisher(pub Publisher) bool {
	if pub == nil {
		return true
	}
	value := reflect.ValueOf(pub)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func (s *Stream) closeGenerationLocked() {
	if s.generationDone == nil || s.generationBoundary == nil || s.generationBoundary.ended.Load() {
		return
	}
	// Frame writes and publisher removal are serialized by s.mu, so this is
	// the exact exclusive upper bound for this generation in the shared ring.
	s.generationBoundary.endCursor.Store(s.ringBuffer.WriteCursor())
	s.generationBoundary.ended.Store(true)
	close(s.generationDone)
}

func (s *Stream) signalStartupStateChangedLocked() {
	close(s.startupStateChanged)
	s.startupStateChanged = make(chan struct{})
}

func (s *Stream) mergePublisherMediaInfoLocked(info *avframe.MediaInfo) {
	if info == nil {
		return
	}
	s.mediaInfo = cloneMediaInfo(*info)
	if len(s.mediaInfo.VideoSequenceHeader) > 0 && s.mediaInfo.VideoCodec != 0 {
		s.videoSeqHeader = avframe.NewAVFrame(
			avframe.MediaTypeVideo,
			s.mediaInfo.VideoCodec,
			avframe.FrameTypeSequenceHeader,
			0,
			0,
			append([]byte(nil), s.mediaInfo.VideoSequenceHeader...),
		)
	}
	if len(s.mediaInfo.AudioSequenceHeader) > 0 && s.mediaInfo.AudioCodec != 0 {
		s.audioSeqHeader = avframe.NewAVFrame(
			avframe.MediaTypeAudio,
			s.mediaInfo.AudioCodec,
			avframe.FrameTypeSequenceHeader,
			0,
			0,
			append([]byte(nil), s.mediaInfo.AudioSequenceHeader...),
		)
	}
	if s.videoSeqHeader != nil || s.audioSeqHeader != nil {
		close(s.seqHeaderReady)
	}
}

func cloneMediaInfo(info avframe.MediaInfo) avframe.MediaInfo {
	info.VideoSequenceHeader = append([]byte(nil), info.VideoSequenceHeader...)
	info.AudioSequenceHeader = append([]byte(nil), info.AudioSequenceHeader...)
	return info
}

func (s *Stream) updateStartupReadyLocked() {
	ready := s.startupReadyLocked()
	if ready == s.startupReady {
		return
	}
	s.startupReady = ready
	s.signalStartupStateChangedLocked()
}

func (s *Stream) startupReadyLocked() bool {
	hasTrack := false
	if codec := s.mediaInfo.VideoCodec; codec != 0 {
		hasTrack = true
		if !trackReady(codec, s.videoSeqHeader != nil || len(s.mediaInfo.VideoSequenceHeader) > 0) {
			return false
		}
	}
	if codec := s.mediaInfo.AudioCodec; codec != 0 {
		hasTrack = true
		if !trackReady(codec, s.audioSeqHeader != nil || len(s.mediaInfo.AudioSequenceHeader) > 0) {
			return false
		}
	}
	return hasTrack
}

func trackReady(codec avframe.CodecType, hasSequenceHeader bool) bool {
	switch codec {
	case avframe.CodecH264,
		avframe.CodecH265,
		avframe.CodecAAC:
		return hasSequenceHeader
	case avframe.CodecMP3,
		avframe.CodecG711A,
		avframe.CodecG711U,
		avframe.CodecG722,
		avframe.CodecG729,
		avframe.CodecAV1,
		avframe.CodecVP8,
		avframe.CodecVP9,
		avframe.CodecOpus:
		return true
	default:
		return false
	}
}

// Close force-closes the stream: closes the ring buffer, removes the publisher,
// and transitions to destroying state.
func (s *Stream) Close() {
	s.mu.Lock()
	if s.state == StreamStateDestroying {
		s.ringBuffer.Close()
		s.mu.Unlock()
		s.notifyDestroy()
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

	publisher := s.publisher
	s.publisher = nil

	s.closeGenerationLocked()
	s.startupReady = false
	s.usedPublisherIDs = nil
	s.state = StreamStateDestroying
	s.signalStartupStateChangedLocked()
	s.ringBuffer.Close()
	s.mu.Unlock()
	if publisher != nil {
		publisher.Close() //nolint:errcheck
	}
	s.notifyDestroy()
}

// LastPublisherID returns the most recent publisher identity. It is used only
// to scope delayed stream-destroy cleanup after the publisher has detached.
func (s *Stream) LastPublisherID() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastPublisherID
}

// LastPublisherGeneration returns the most recent publisher generation after
// the publisher has detached.
func (s *Stream) LastPublisherGeneration() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastPublisherGeneration
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
	if frame == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeFrameLocked(frame)
}

// WriteFrameForPublisher writes a frame only when pub still owns the active generation.
func (s *Stream) WriteFrameForPublisher(pub Publisher, frame *avframe.AVFrame) bool {
	if frame == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.publisherMatchesLocked(pub) || s.state != StreamStatePublishing {
		return false
	}
	return s.writeFrameLocked(frame)
}

// WithActivePublisher runs activity while pub owns the active publishing
// generation. activity executes with the stream lock held and must not call
// any Stream method.
func (s *Stream) WithActivePublisher(pub Publisher, activity func()) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.publisherMatchesLocked(pub) || s.state != StreamStatePublishing {
		return false
	}
	activity()
	return true
}

func (s *Stream) writeFrameLocked(frame *avframe.AVFrame) bool {
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
	if frame.MediaType.IsVideo() {
		s.mediaInfo.VideoCodec = frame.Codec
	} else if frame.MediaType.IsAudio() {
		if s.audioCodecEpoch == 0 || s.mediaInfo.AudioCodec != frame.Codec {
			s.audioCodecEpoch++
		}
		s.mediaInfo.AudioCodec = frame.Codec
		switch frame.Codec {
		case avframe.CodecG711A, avframe.CodecG711U:
			// G.711 has a fixed RTP clock and mono channel layout. Some
			// publishers, such as the GB28181 PS demuxer, discover the
			// codec after SetPublisher and cannot provide these fields up
			// front; keep the stream startup snapshot self-consistent.
			s.mediaInfo.SampleRate = 8000
			s.mediaInfo.Channels = 1
		}
		frame.AudioCodecEpoch = s.audioCodecEpoch
		frame.AudioProvenance = avframe.FrameProvenanceSource
	}
	if frame.FrameType == avframe.FrameTypeSequenceHeader {
		if frame.MediaType.IsVideo() {
			s.videoSeqHeader = frame
			s.mediaInfo.VideoSequenceHeader = append([]byte(nil), frame.Payload...)
		} else if frame.MediaType.IsAudio() {
			s.audioSeqHeader = frame
			s.mediaInfo.AudioSequenceHeader = append([]byte(nil), frame.Payload...)
		}
		// Signal waiters that at least one sequence header is available.
		select {
		case <-s.seqHeaderReady:
			// Already closed
		default:
			close(s.seqHeaderReady)
		}
	}
	s.updateStartupReadyLocked()

	// Update GOP cache for video frames
	if s.config.GOPCache && s.config.GOPCacheNum > 0 {
		if frame.MediaType.IsVideo() {
			if frame.FrameType.IsKeyframe() {
				// Start new GOP
				s.gopGeneration++
				s.gopCacheSealed = false
				gopStart := s.ringBuffer.WriteCursor()
				s.gopCache = append(s.gopCache, []*avframe.AVFrame{frame})
				s.gopStarts = append(s.gopStarts, gopStart)
				s.gopBytes = append(s.gopBytes, int64(len(frame.Payload)))
				s.gopMinDTS = append(s.gopMinDTS, frame.DTS)
				s.gopMaxDTS = append(s.gopMaxDTS, frame.DTS)
				if len(s.gopCache) > s.config.GOPCacheNum {
					s.gopCache = s.gopCache[len(s.gopCache)-s.config.GOPCacheNum:]
					s.gopStarts = s.gopStarts[len(s.gopStarts)-s.config.GOPCacheNum:]
					s.gopBytes = s.gopBytes[len(s.gopBytes)-s.config.GOPCacheNum:]
					s.gopMinDTS = s.gopMinDTS[len(s.gopMinDTS)-s.config.GOPCacheNum:]
					s.gopMaxDTS = s.gopMaxDTS[len(s.gopMaxDTS)-s.config.GOPCacheNum:]
				}
			} else if frame.FrameType != avframe.FrameTypeSequenceHeader && len(s.gopCache) > 0 {
				s.appendGOPFrameLocked(frame)
			}
		} else if frame.MediaType.IsAudio() && frame.FrameType != avframe.FrameTypeSequenceHeader && len(s.gopCache) > 0 {
			// Interleave audio into GOP cache for DTS ordering
			s.appendGOPFrameLocked(frame)
		}
	}

	s.stats.recordFrame(len(frame.Payload), frame.MediaType.IsVideo())
	s.ringBuffer.Write(frame)
	return true
}

// appendGOPFrameLocked adds a frame to the current GOP when doing so stays
// within every configured per-GOP bound. The keyframe that starts a GOP is
// always retained, even when its payload alone exceeds max bytes.
func (s *Stream) appendGOPFrameLocked(frame *avframe.AVFrame) {
	if len(s.gopCache) == 0 || s.gopCacheSealed {
		return
	}
	index := len(s.gopCache) - 1
	gop := s.gopCache[index]
	if s.config.GOPCacheMaxFrames > 0 && len(gop) >= s.config.GOPCacheMaxFrames {
		s.gopCacheSealed = true
		return
	}
	if s.config.GOPCacheMaxBytes > 0 && len(gop) > 0 && s.gopBytes[index]+int64(len(frame.Payload)) > s.config.GOPCacheMaxBytes {
		s.gopCacheSealed = true
		return
	}
	minDTS, maxDTS := s.gopMinDTS[index], s.gopMaxDTS[index]
	if frame.DTS < minDTS {
		minDTS = frame.DTS
	}
	if frame.DTS > maxDTS {
		maxDTS = frame.DTS
	}
	if s.config.GOPCacheMaxDuration > 0 && len(gop) > 0 && gopDurationExceeded(minDTS, maxDTS, s.config.GOPCacheMaxDuration) {
		s.gopCacheSealed = true
		return
	}
	s.gopCache[index] = append(gop, frame)
	s.gopBytes[index] += int64(len(frame.Payload))
	s.gopMinDTS[index] = minDTS
	s.gopMaxDTS[index] = maxDTS
	if s.currentGOPAtBoundLocked() {
		s.gopCacheSealed = true
	}
}

func (s *Stream) currentGOPAtBoundLocked() bool {
	if len(s.gopCache) == 0 {
		return false
	}
	index := len(s.gopCache) - 1
	gop := s.gopCache[index]
	if s.config.GOPCacheMaxFrames > 0 && len(gop) >= s.config.GOPCacheMaxFrames {
		return true
	}
	if s.config.GOPCacheMaxBytes > 0 && s.gopBytes[index] >= s.config.GOPCacheMaxBytes {
		return true
	}
	return s.config.GOPCacheMaxDuration > 0 && len(gop) > 0 &&
		gopDurationAtLeast(s.gopMinDTS[index], s.gopMaxDTS[index], s.config.GOPCacheMaxDuration)
}

func gopDurationExceeded(minDTS, maxDTS int64, limit time.Duration) bool {
	if limit <= 0 || maxDTS < minDTS {
		return false
	}
	return dtsSpanMillis(minDTS, maxDTS) > int64(limit/time.Millisecond)
}

func gopDurationAtLeast(minDTS, maxDTS int64, limit time.Duration) bool {
	if limit <= 0 || maxDTS < minDTS {
		return false
	}
	return dtsSpanMillis(minDTS, maxDTS) >= int64(limit/time.Millisecond)
}

func dtsSpanMillis(minDTS, maxDTS int64) int64 {
	if maxDTS < minDTS {
		minDTS, maxDTS = maxDTS, minDTS
	}
	if minDTS < 0 && maxDTS > (1<<63-1)+minDTS {
		return 1<<63 - 1
	}
	return maxDTS - minDTS
}

// trimGOPCacheLocked repairs cache entries after a policy update. It keeps a
// playable prefix beginning at each GOP keyframe and never trims a keyframe.
func (s *Stream) trimGOPCacheLocked() {
	s.gopCacheSealed = false
	if !s.config.GOPCache || s.config.GOPCacheNum <= 0 {
		s.gopCache = nil
		s.gopStarts = nil
		s.gopBytes = nil
		s.gopMinDTS = nil
		s.gopMaxDTS = nil
		return
	}
	if len(s.gopCache) > s.config.GOPCacheNum {
		start := len(s.gopCache) - s.config.GOPCacheNum
		s.gopCache = s.gopCache[start:]
		s.gopStarts = s.gopStarts[start:]
		s.gopBytes = s.gopBytes[start:]
		s.gopMinDTS = s.gopMinDTS[start:]
		s.gopMaxDTS = s.gopMaxDTS[start:]
	}
	for i, gop := range s.gopCache {
		if len(gop) == 0 {
			s.gopBytes[i] = 0
			s.gopMinDTS[i] = 0
			s.gopMaxDTS[i] = 0
			continue
		}
		limit := len(gop)
		if s.config.GOPCacheMaxFrames > 0 && limit > s.config.GOPCacheMaxFrames {
			limit = s.config.GOPCacheMaxFrames
		}
		if s.config.GOPCacheMaxBytes > 0 {
			bytes := int64(0)
			byteLimit := 0
			for n, frame := range gop[:limit] {
				frameBytes := int64(len(frame.Payload))
				if n > 0 && bytes+frameBytes > s.config.GOPCacheMaxBytes {
					break
				}
				bytes += frameBytes
				byteLimit = n + 1
				if bytes >= s.config.GOPCacheMaxBytes {
					break
				}
			}
			if byteLimit < limit {
				limit = byteLimit
			}
		}
		if s.config.GOPCacheMaxDuration > 0 {
			durationLimit := 1
			minDTS, maxDTS := gop[0].DTS, gop[0].DTS
			for n := 1; n < limit; n++ {
				candidateMin, candidateMax := minDTS, maxDTS
				if gop[n].DTS < candidateMin {
					candidateMin = gop[n].DTS
				}
				if gop[n].DTS > candidateMax {
					candidateMax = gop[n].DTS
				}
				if gopDurationExceeded(candidateMin, candidateMax, s.config.GOPCacheMaxDuration) {
					break
				}
				minDTS, maxDTS = candidateMin, candidateMax
				durationLimit = n + 1
			}
			if durationLimit < limit {
				limit = durationLimit
			}
		}
		if limit < 1 {
			limit = 1
		}
		if i == len(s.gopCache)-1 && limit < len(gop) {
			s.gopCacheSealed = true
		}
		s.gopCache[i] = gop[:limit]
		bytes := int64(0)
		for _, frame := range s.gopCache[i] {
			bytes += int64(len(frame.Payload))
		}
		s.gopBytes[i] = bytes
		minDTS, maxDTS := s.gopCache[i][0].DTS, s.gopCache[i][0].DTS
		for _, frame := range s.gopCache[i][1:] {
			if frame.DTS < minDTS {
				minDTS = frame.DTS
			}
			if frame.DTS > maxDTS {
				maxDTS = frame.DTS
			}
		}
		s.gopMinDTS[i], s.gopMaxDTS[i] = minDTS, maxDTS
	}
	if s.currentGOPAtBoundLocked() {
		s.gopCacheSealed = true
	}
}

// StreamStartupSnapshot is an atomic view of the current publisher generation's startup state.
type StreamStartupSnapshot struct {
	StreamInstanceID      uint64
	Generation            uint64
	PublisherID           string
	GenerationStartCursor int64
	MediaInfo             avframe.MediaInfo
	VideoSequenceHeader   *avframe.AVFrame
	AudioSequenceHeader   *avframe.AVFrame
	ReplayFrames          []*avframe.AVFrame
	LiveCursor            int64
	SourceCursor          int64
	GenerationDone        <-chan struct{}
	Ready                 bool
	audioCodecEpoch       uint64
	generationBoundary    *streamGenerationBoundary
}

// GenerationEndCursor returns the exclusive source-ring boundary captured
// when this snapshot's publisher generation ended.
func (s StreamStartupSnapshot) GenerationEndCursor() (int64, bool) {
	return s.generationBoundary.end()
}

// StartupSnapshot captures media information, headers, replay frames, and cursors atomically.
func (s *Stream) StartupSnapshot() StreamStartupSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.startupSnapshotLocked()
}

func (s *Stream) startupSnapshotLocked() StreamStartupSnapshot {
	liveCursor := s.ringBuffer.WriteCursor()
	sourceCursor := liveCursor
	if len(s.gopStarts) > 0 {
		sourceCursor = s.gopStarts[0]
	}
	var replayFrames []*avframe.AVFrame
	for _, gop := range s.gopCache {
		replayFrames = append(replayFrames, gop...)
	}
	publisherID := ""
	if !isNilPublisher(s.publisher) {
		publisherID = s.publisher.ID()
	}
	return StreamStartupSnapshot{
		StreamInstanceID:      s.instanceID,
		Generation:            s.publisherGeneration,
		PublisherID:           publisherID,
		GenerationStartCursor: s.generationStartCursor,
		MediaInfo:             cloneMediaInfo(s.mediaInfo),
		VideoSequenceHeader:   s.videoSeqHeader,
		AudioSequenceHeader:   s.audioSeqHeader,
		ReplayFrames:          replayFrames,
		LiveCursor:            liveCursor,
		SourceCursor:          sourceCursor,
		GenerationDone:        s.generationDone,
		Ready:                 s.startupReady,
		audioCodecEpoch:       s.audioCodecEpoch,
		generationBoundary:    s.generationBoundary,
	}
}

// WaitForStartup waits until the current generation is publishing and ready.
func (s *Stream) WaitForStartup(ctx context.Context) (StreamStartupSnapshot, bool) {
	for {
		if ctx.Err() != nil {
			return StreamStartupSnapshot{}, false
		}
		s.mu.RLock()
		if s.state == StreamStatePublishing && s.startupReady {
			snapshot := s.startupSnapshotLocked()
			s.mu.RUnlock()
			return snapshot, true
		}
		changed := s.startupStateChanged
		s.mu.RUnlock()

		select {
		case <-ctx.Done():
			return StreamStartupSnapshot{}, false
		case <-changed:
		}
	}
}

// IsPublisherGeneration reports whether generation is the active publishing generation.
func (s *Stream) IsPublisherGeneration(generation uint64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state == StreamStatePublishing && s.publisherGeneration == generation
}

func (s *Stream) activePublisherGeneration() (uint64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.state != StreamStatePublishing || isNilPublisher(s.publisher) {
		return 0, false
	}
	return s.publisherGeneration, true
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
	Generation  uint64
}

func (s *Stream) GOPCacheDetail() GOPCacheDetail {
	s.mu.RLock()
	defer s.mu.RUnlock()

	d := GOPCacheDetail{Generation: s.gopGeneration}
	for _, gop := range s.gopCache {
		d.TotalFrames += len(gop)
	}
	if d.TotalFrames == 0 {
		return d
	}

	var minDTS, maxDTS int64
	dtsSet := false
	for _, gop := range s.gopCache {
		for _, f := range gop {
			if f.MediaType.IsVideo() {
				d.VideoFrames++
			} else if f.MediaType.IsAudio() {
				d.AudioFrames++
			}
			if !dtsSet {
				minDTS = f.DTS
				maxDTS = f.DTS
				dtsSet = true
			} else {
				if f.DTS < minDTS {
					minDTS = f.DTS
				}
				if f.DTS > maxDTS {
					maxDTS = f.DTS
				}
			}
		}
	}
	if dtsSet {
		d.DurationMs = dtsSpanMillis(minDTS, maxDTS)
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

// GOPCacheSnapshot returns a flattened copy of all cached GOPs together
// with the ring-buffer write cursor, captured atomically under the stream
// lock. Subscribers must send the returned frames first and then read the
// ring buffer starting at the returned cursor; capturing the two values
// separately allows the publisher to write frames in between, which would
// then be delivered twice (once from the GOP cache, once from the ring)
// and break DTS monotonicity.
func (s *Stream) GOPCacheSnapshot() ([]*avframe.AVFrame, int64) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []*avframe.AVFrame
	for _, gop := range s.gopCache {
		result = append(result, gop...)
	}
	return result, s.ringBuffer.WriteCursor()
}

// GOPCacheSourceStart returns the ring-buffer position of the oldest cached
// GOP. A snapshot subscriber that needs to transform cached media can start
// its source reader here, while the normal live reader still starts at the
// atomic cursor returned by GOPCacheSnapshot.
func (s *Stream) GOPCacheSourceStart() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.gopStarts) == 0 {
		return s.ringBuffer.WriteCursor()
	}
	return s.gopStarts[0]
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

func (s *Stream) audioCodecState() (avframe.CodecType, uint64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.mediaInfo.AudioCodec, s.audioCodecEpoch
}

// SeqHeaderReady returns a channel that is closed when the first sequence header
// (video or audio) is stored. Subscribers can select on this instead of polling.
func (s *Stream) SeqHeaderReady() <-chan struct{} {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.seqHeaderReady
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

// FeedbackRouter returns the stream's feedback router.
func (s *Stream) FeedbackRouter() *FeedbackRouter {
	return s.feedbackRouter
}

// TranscodeManager returns the stream's audio transcoding manager, or nil if disabled.
func (s *Stream) TranscodeManager() *TranscodeManager {
	return s.transcodeManager
}

// AddSubscriber increments the subscriber count for a protocol (e.g. "rtmp").
// Returns an error if max_subscribers_per_stream limit is reached.
func (s *Stream) AddSubscriber(protocol string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addSubscriberLocked(protocol)
}

func (s *Stream) addSubscriberLocked(protocol string) error {
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

	// Update feedback router with new subscriber count
	s.feedbackRouter.SetSubscriberCount(s.totalSubscribers())

	// Cancel idle timer — we have a subscriber now
	if s.idleTimer != nil {
		s.idleTimer.Stop()
		s.idleTimer = nil
	}

	return nil
}

// AddSubscriberForGeneration admits a subscriber and returns a release function
// that can only remove the lease created for this publisher generation.
func (s *Stream) AddSubscriberForGeneration(protocol string, generation uint64) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != StreamStatePublishing || s.publisherGeneration != generation {
		return nil, fmt.Errorf("publisher generation %d is no longer active", generation)
	}
	if err := s.addSubscriberLocked(protocol); err != nil {
		return nil, err
	}
	byProtocol := s.generationSubscribers[generation]
	if byProtocol == nil {
		byProtocol = make(map[string]int)
		s.generationSubscribers[generation] = byProtocol
	}
	byProtocol[protocol]++

	var once sync.Once
	return func() {
		once.Do(func() { s.removeSubscriberForGeneration(protocol, generation) })
	}, nil
}

func (s *Stream) removeSubscriberForGeneration(protocol string, generation uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	byProtocol := s.generationSubscribers[generation]
	if byProtocol == nil || byProtocol[protocol] <= 0 {
		return
	}
	byProtocol[protocol]--
	if byProtocol[protocol] == 0 {
		delete(byProtocol, protocol)
	}
	if len(byProtocol) == 0 {
		delete(s.generationSubscribers, generation)
	}
	if s.subscribers[protocol] > 0 {
		s.subscribers[protocol]--
		if s.subscribers[protocol] == 0 {
			delete(s.subscribers, protocol)
		}
	}
	s.feedbackRouter.SetSubscriberCount(s.totalSubscribers())
	s.checkIdleTimeout()
}

// RemoveSubscriber decrements the subscriber count for a protocol.
func (s *Stream) RemoveSubscriber(protocol string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subscribers[protocol]--
	if s.subscribers[protocol] <= 0 {
		delete(s.subscribers, protocol)
	}
	s.feedbackRouter.SetSubscriberCount(s.totalSubscribers())
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
				notify := false
				if s.publisher == nil && s.totalSubscribers() == 0 {
					s.usedPublisherIDs = nil
					s.state = StreamStateDestroying
					s.signalStartupStateChangedLocked()
					notify = true
				}
				s.mu.Unlock()
				if notify {
					s.notifyDestroy()
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
