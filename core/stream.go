package core

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
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
	key    string
	config config.StreamConfig
	limits config.LimitsConfig

	mu        sync.RWMutex
	state     StreamState
	publisher Publisher

	ringBuffer    *util.RingBuffer[*avframe.AVFrame]
	muxerManager  *MuxerManager
	gopCache      [][]*avframe.AVFrame
	gopStarts     []int64
	gopGeneration uint64
	audioCache    []*avframe.AVFrame
	subscribers   map[string]int // protocol -> count (e.g. "rtmp" -> 2)

	videoSeqHeader *avframe.AVFrame
	audioSeqHeader *avframe.AVFrame
	seqHeaderReady chan struct{} // closed when first sequence header arrives

	publisherGeneration   uint64
	generationStartCursor int64
	mediaInfo             avframe.MediaInfo
	generationDone        chan struct{}
	startupStateChanged   chan struct{}
	startupReady          bool

	stats            StreamStats
	eventBus         *EventBus
	noPublisherTimer *time.Timer
	idleTimer        *time.Timer
	feedbackRouter   *FeedbackRouter
	transcodeManager *TranscodeManager
}

// NewStream creates a new Stream in idle state.
func NewStream(key string, cfg config.StreamConfig, limits config.LimitsConfig, bus *EventBus) *Stream {
	s := &Stream{
		key:                 key,
		config:              cfg,
		limits:              limits,
		state:               StreamStateIdle,
		ringBuffer:          util.NewRingBuffer[*avframe.AVFrame](cfg.RingBufferSize),
		eventBus:            bus,
		subscribers:         make(map[string]int),
		seqHeaderReady:      make(chan struct{}),
		startupStateChanged: make(chan struct{}),
	}
	s.muxerManager = NewMuxerManager(s, cfg.RingBufferSize)
	s.feedbackRouter = NewFeedbackRouter(cfg.Feedback)
	return s
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
	s.mu.Lock()
	s.config = cfg
	s.limits = limits
	if !cfg.GOPCache || cfg.GOPCacheNum <= 0 {
		s.gopCache = nil
		s.gopStarts = nil
	} else if len(s.gopCache) > cfg.GOPCacheNum {
		s.gopCache = append([][]*avframe.AVFrame(nil), s.gopCache[len(s.gopCache)-cfg.GOPCacheNum:]...)
		s.gopStarts = append([]int64(nil), s.gopStarts[len(s.gopStarts)-cfg.GOPCacheNum:]...)
	}
	if cfg.AudioCacheMs <= 0 {
		s.audioCache = nil
	} else if len(s.audioCache) > 0 {
		cutoff := s.audioCache[len(s.audioCache)-1].DTS - int64(cfg.AudioCacheMs)
		first := 0
		for first < len(s.audioCache) && s.audioCache[first].DTS < cutoff {
			first++
		}
		if first > 0 {
			s.audioCache = append([]*avframe.AVFrame(nil), s.audioCache[first:]...)
		}
	}
	if s.noPublisherTimer != nil {
		s.noPublisherTimer.Stop()
		s.noPublisherTimer = nil
	}
	if s.state == StreamStateNoPublisher && cfg.NoPublisherTimeout > 0 {
		s.noPublisherTimer = time.AfterFunc(cfg.NoPublisherTimeout, func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.state == StreamStateNoPublisher {
				s.state = StreamStateDestroying
				s.signalStartupStateChangedLocked()
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

	if s.transcodeManager != nil {
		s.transcodeManager.Reset()
	}

	s.closeGenerationLocked()
	s.publisherGeneration++
	s.generationStartCursor = s.ringBuffer.WriteCursor()
	s.gopCache = nil
	s.gopStarts = nil
	s.gopGeneration = 0
	s.videoSeqHeader = nil
	s.audioSeqHeader = nil
	s.seqHeaderReady = make(chan struct{})
	s.mediaInfo = avframe.MediaInfo{}
	s.startupReady = false
	s.generationDone = make(chan struct{})
	if pub != nil {
		s.mergePublisherMediaInfoLocked(pub.MediaInfo())
	}
	s.publisher = pub
	s.state = StreamStatePublishing
	s.startupReady = s.startupReadyLocked()
	s.stats.initStats()
	s.signalStartupStateChangedLocked()

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
	if !samePublisher(s.publisher, pub) {
		return false
	}
	s.removePublisherLocked()
	return true
}

func (s *Stream) removePublisherLocked() {
	s.closeGenerationLocked()
	s.publisher = nil
	s.state = StreamStateNoPublisher
	s.startupReady = false
	s.signalStartupStateChangedLocked()

	if s.config.NoPublisherTimeout > 0 {
		s.noPublisherTimer = time.AfterFunc(s.config.NoPublisherTimeout, func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.state == StreamStateNoPublisher {
				s.state = StreamStateDestroying
				s.signalStartupStateChangedLocked()
			}
		})
	}

	s.checkIdleTimeout()
}

func samePublisher(left, right Publisher) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	leftType := reflect.TypeOf(left)
	if leftType != reflect.TypeOf(right) || !leftType.Comparable() {
		return false
	}
	return left == right
}

func (s *Stream) closeGenerationLocked() {
	if s.generationDone == nil {
		return
	}
	select {
	case <-s.generationDone:
	default:
		close(s.generationDone)
	}
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
		avframe.CodecAV1,
		avframe.CodecVP8,
		avframe.CodecVP9,
		avframe.CodecAAC,
		avframe.CodecOpus:
		return hasSequenceHeader
	case avframe.CodecMP3,
		avframe.CodecG711A,
		avframe.CodecG711U,
		avframe.CodecG722,
		avframe.CodecG729:
		return true
	default:
		return false
	}
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

	s.closeGenerationLocked()
	s.startupReady = false
	s.state = StreamStateDestroying
	s.signalStartupStateChangedLocked()
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
	return s.writeFrameLocked(frame)
}

// WriteFrameForPublisher writes a frame only when pub still owns the active generation.
func (s *Stream) WriteFrameForPublisher(pub Publisher, frame *avframe.AVFrame) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !samePublisher(s.publisher, pub) || s.state != StreamStatePublishing {
		return false
	}
	return s.writeFrameLocked(frame)
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
		s.mediaInfo.AudioCodec = frame.Codec
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
	if s.config.GOPCache {
		if frame.MediaType.IsVideo() {
			if frame.FrameType.IsKeyframe() {
				// Start new GOP
				s.gopGeneration++
				gopStart := s.ringBuffer.WriteCursor()
				s.gopCache = append(s.gopCache, []*avframe.AVFrame{frame})
				s.gopStarts = append(s.gopStarts, gopStart)
				if len(s.gopCache) > s.config.GOPCacheNum {
					s.gopCache = s.gopCache[len(s.gopCache)-s.config.GOPCacheNum:]
					s.gopStarts = s.gopStarts[len(s.gopStarts)-s.config.GOPCacheNum:]
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

// StreamStartupSnapshot is an atomic view of the current publisher generation's startup state.
type StreamStartupSnapshot struct {
	Generation            uint64
	GenerationStartCursor int64
	MediaInfo             avframe.MediaInfo
	VideoSequenceHeader   *avframe.AVFrame
	AudioSequenceHeader   *avframe.AVFrame
	ReplayFrames          []*avframe.AVFrame
	LiveCursor            int64
	SourceCursor          int64
	GenerationDone        <-chan struct{}
	Ready                 bool
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
	return StreamStartupSnapshot{
		Generation:            s.publisherGeneration,
		GenerationStartCursor: s.generationStartCursor,
		MediaInfo:             cloneMediaInfo(s.mediaInfo),
		VideoSequenceHeader:   s.videoSeqHeader,
		AudioSequenceHeader:   s.audioSeqHeader,
		ReplayFrames:          replayFrames,
		LiveCursor:            liveCursor,
		SourceCursor:          sourceCursor,
		GenerationDone:        s.generationDone,
		Ready:                 s.startupReady,
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

func (s *Stream) currentPublisherGeneration() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.publisherGeneration
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
		d.DurationMs = maxDTS - minDTS
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

// AudioCache returns a copy of the current audio cache.
func (s *Stream) AudioCache() []*avframe.AVFrame {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]*avframe.AVFrame, len(s.audioCache))
	copy(result, s.audioCache)
	return result
}

// AudioCacheDetail returns audio-cache statistics without copying frames.
type AudioCacheDetail struct {
	Frames     int
	DurationMs int64
}

func (s *Stream) AudioCacheDetail() AudioCacheDetail {
	s.mu.RLock()
	defer s.mu.RUnlock()

	detail := AudioCacheDetail{Frames: len(s.audioCache)}
	if detail.Frames > 1 {
		detail.DurationMs = s.audioCache[detail.Frames-1].DTS - s.audioCache[0].DTS
	}
	return detail
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
