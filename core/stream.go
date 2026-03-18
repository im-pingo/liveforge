package core

import (
	"errors"
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

// Stream manages the lifecycle, publisher, subscribers, and frame distribution for a stream key.
type Stream struct {
	key    string
	config config.StreamConfig

	mu        sync.RWMutex
	state     StreamState
	publisher Publisher

	ringBuffer   *util.RingBuffer[*avframe.AVFrame]
	muxerManager *MuxerManager
	gopCache     []*avframe.AVFrame
	subscribers  map[string]int // protocol -> count (e.g. "rtmp" -> 2)

	videoSeqHeader *avframe.AVFrame
	audioSeqHeader *avframe.AVFrame

	eventBus         *EventBus
	noPublisherTimer *time.Timer
}

// NewStream creates a new Stream in idle state.
func NewStream(key string, cfg config.StreamConfig, bus *EventBus) *Stream {
	s := &Stream{
		key:         key,
		config:      cfg,
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

	s.publisher = pub
	s.state = StreamStatePublishing

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
}

// Publisher returns the current publisher, if any.
func (s *Stream) Publisher() Publisher {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.publisher
}

// WriteFrame writes a media frame to the ring buffer and updates caches.
func (s *Stream) WriteFrame(frame *avframe.AVFrame) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Store sequence headers separately for late-joining subscribers
	if frame.FrameType == avframe.FrameTypeSequenceHeader {
		if frame.MediaType.IsVideo() {
			s.videoSeqHeader = frame
		} else if frame.MediaType.IsAudio() {
			s.audioSeqHeader = frame
		}
	}

	// Update GOP cache for video frames
	if frame.MediaType.IsVideo() {
		if frame.FrameType.IsKeyframe() {
			// Start new GOP
			s.gopCache = []*avframe.AVFrame{frame}
		} else if frame.FrameType != avframe.FrameTypeSequenceHeader && len(s.gopCache) > 0 {
			s.gopCache = append(s.gopCache, frame)
		}
	} else if frame.MediaType.IsAudio() && frame.FrameType != avframe.FrameTypeSequenceHeader && len(s.gopCache) > 0 {
		// Interleave audio into GOP cache for DTS ordering
		s.gopCache = append(s.gopCache, frame)
	}

	s.ringBuffer.Write(frame)
}

// GOPCacheLen returns the number of frames in the current GOP cache.
func (s *Stream) GOPCacheLen() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.gopCache)
}

// GOPCache returns a copy of the current GOP cache.
func (s *Stream) GOPCache() []*avframe.AVFrame {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]*avframe.AVFrame, len(s.gopCache))
	copy(result, s.gopCache)
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

// RingBuffer returns the stream's ring buffer for reader creation.
func (s *Stream) RingBuffer() *util.RingBuffer[*avframe.AVFrame] {
	return s.ringBuffer
}

// MuxerManager returns the stream's muxer manager.
func (s *Stream) MuxerManager() *MuxerManager {
	return s.muxerManager
}

// AddSubscriber increments the subscriber count for a protocol (e.g. "rtmp").
func (s *Stream) AddSubscriber(protocol string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subscribers[protocol]++
}

// RemoveSubscriber decrements the subscriber count for a protocol.
func (s *Stream) RemoveSubscriber(protocol string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subscribers[protocol]--
	if s.subscribers[protocol] <= 0 {
		delete(s.subscribers, protocol)
	}
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
