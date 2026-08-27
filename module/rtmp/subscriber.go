package rtmp

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync/atomic"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	flvpkg "github.com/im-pingo/liveforge/pkg/muxer/flv"
	"github.com/im-pingo/liveforge/pkg/util"
)

// Subscriber implements core.Subscriber for RTMP connections.
type Subscriber struct {
	id        string
	conn      net.Conn
	cw        *ChunkWriter
	stream    *core.Stream
	opts      core.SubscribeOptions
	skipCfg   *config.SkipTrackerConfig
	caps      PeerCapabilities
	onFailure func(error)
	closed    chan struct{}
	startup   *core.StreamStartupSnapshot

	// Reusable per-frame encoding state to avoid heap allocations on the hot path.
	flvBuf bytes.Buffer
	muxer  *flvpkg.Muxer
}

var subscriberSequence atomic.Uint64

// NewSubscriber creates a new RTMP subscriber.
func NewSubscriber(streamKey string, conn net.Conn, cw *ChunkWriter, stream *core.Stream, skipCfg *config.SkipTrackerConfig) *Subscriber {
	return NewSubscriberWithCapabilities(streamKey, conn, cw, stream, skipCfg, PeerCapabilities{}, nil)
}

// NewSubscriberWithCapabilities creates an RTMP subscriber with peer codec
// negotiation state.
func NewSubscriberWithCapabilities(streamKey string, conn net.Conn, cw *ChunkWriter, stream *core.Stream, skipCfg *config.SkipTrackerConfig, caps PeerCapabilities, onFailure func(error)) *Subscriber {
	return &Subscriber{
		id:        fmt.Sprintf("rtmp-sub-%s-%d", streamKey, subscriberSequence.Add(1)),
		conn:      conn,
		cw:        cw,
		stream:    stream,
		opts:      core.DefaultSubscribeOptions(),
		skipCfg:   skipCfg,
		caps:      caps,
		onFailure: onFailure,
		closed:    make(chan struct{}),
		muxer:     flvpkg.NewMuxer(),
	}
}

// ID returns the subscriber identifier.
func (s *Subscriber) ID() string { return s.id }

// Options returns the subscribe options.
func (s *Subscriber) Options() core.SubscribeOptions { return s.opts }

// OnData receives muxed data and writes it over the RTMP connection.
func (s *Subscriber) OnData(data []byte) error {
	// data contains an FLV tag — extract and send as RTMP message
	return nil
}

// Close disconnects the subscriber.
func (s *Subscriber) Close() error {
	select {
	case <-s.closed:
	default:
		close(s.closed)
	}
	return s.conn.Close()
}

// WriteLoop reads frames from the stream ring buffer and sends them as RTMP chunks.
func (s *Subscriber) WriteLoop() {
	defer s.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-s.closed:
			cancel()
		case <-ctx.Done():
		}
	}()
	var snapshot core.StreamStartupSnapshot
	if s.startup != nil {
		snapshot = *s.startup
	} else {
		pending := s.stream.StartupSnapshot()
		if pending.GenerationDone != nil {
			go func() {
				select {
				case <-pending.GenerationDone:
					cancel()
				case <-ctx.Done():
				}
			}()
		}
		var ok bool
		snapshot, ok = s.stream.WaitForStartup(ctx)
		if !ok || (pending.Generation != 0 && snapshot.Generation != pending.Generation) {
			return
		}
	}
	if !snapshot.Ready || !s.stream.IsPublisherGeneration(snapshot.Generation) {
		return
	}

	policy, err := chooseOutputPolicy(&snapshot.MediaInfo, s.caps)
	if err != nil {
		s.fail(err)
		return
	}
	s.muxer = flvpkg.NewMuxerWithModes(policy.videoMode, policy.audioMode)

	var transcodeRelease func()
	needsTranscode := policy.transcodeAudio

	// Send sequence headers
	if vsh := snapshot.VideoSequenceHeader; vsh != nil {
		if err := s.sendFrame(vsh); err != nil {
			slog.Error("video seq header send error", "module", "rtmp", "subscriber", s.id, "error", err)
			return
		}
	}
	if !needsTranscode {
		if ash := snapshot.AudioSequenceHeader; ash != nil {
			if err := s.sendFrame(ash); err != nil {
				slog.Error("audio seq header send error", "module", "rtmp", "subscriber", s.id, "error", err)
				return
			}
		}
	}

	// Send GOP cache if in GOP mode
	if s.opts.StartMode == core.StartModeGOP {
		for _, frame := range snapshot.ReplayFrames {
			if !s.stream.IsPublisherGeneration(snapshot.Generation) {
				return
			}
			// Skip audio from GOP cache when transcoding; transcoded audio
			// comes from the TranscodeManager reader.
			if needsTranscode && frame.MediaType.IsAudio() {
				continue
			}
			if err := s.sendFrame(frame); err != nil {
				slog.Error("GOP cache send error", "module", "rtmp", "subscriber", s.id, "error", err)
				return
			}
		}
	}

	// Set up the live reader. The legacy transcode reader provides source video
	// passthrough together with target audio for RTMP subscribers; it starts at
	// the post-snapshot cursor so cached video is not replayed twice.
	var reader *util.RingReader[*avframe.AVFrame]
	if needsTranscode {
		if tm := s.stream.TranscodeManager(); tm != nil {
			var err error
			reader, transcodeRelease, err = tm.GetOrCreateReaderAt(avframe.CodecAAC, snapshot.SourceCursor)
			if err != nil {
				s.fail(fmt.Errorf("rtmp: audio transcode unavailable: %w", err))
				return
			}
		} else {
			s.fail(fmt.Errorf("rtmp: audio transcode unavailable"))
			return
		}
	} else {
		reader = s.stream.RingBuffer().NewReaderAt(snapshot.LiveCursor)
	}
	if transcodeRelease != nil {
		defer transcodeRelease()
	}

	filter := core.NewSlowConsumerFilter(reader, s.stream.Config().SlowConsumer, s.skipCfg)

	// Watch for subscriber close and unblock any in-progress Read().
	go func() {
		select {
		case <-s.closed:
		case <-snapshot.GenerationDone:
		}
		filter.Close()
	}()

	for {
		frame, ok := filter.NextFrame()
		if !ok {
			return
		}
		if !s.stream.IsPublisherGeneration(snapshot.Generation) {
			return
		}

		start := time.Now()
		if err := s.sendFrame(frame); err != nil {
			return
		}
		filter.ReportSendTime(time.Since(start))
	}
}

func (s *Subscriber) buildRTMPPayload(frame *avframe.AVFrame) ([]byte, error) {
	s.flvBuf.Reset()
	if err := s.muxer.WriteFrame(&s.flvBuf, frame); err != nil {
		return nil, err
	}

	// Extract the FLV tag body (skip tag header and trailing prev tag size)
	tagData := s.flvBuf.Bytes()
	if len(tagData) < flvpkg.TagHeaderSize+4 {
		return nil, nil
	}

	dataSize := len(tagData) - flvpkg.TagHeaderSize - 4
	if dataSize <= 0 {
		return nil, nil
	}
	return tagData[flvpkg.TagHeaderSize : flvpkg.TagHeaderSize+dataSize], nil
}

func (s *Subscriber) fail(err error) {
	if s.onFailure != nil {
		s.onFailure(err)
	}
}

func (s *Subscriber) sendFrame(frame *avframe.AVFrame) error {
	payload, err := s.buildRTMPPayload(frame)
	if err != nil {
		return err
	}
	if payload == nil {
		return nil
	}

	var msgTypeID uint8
	if frame.MediaType.IsVideo() {
		msgTypeID = MsgVideo
	} else {
		msgTypeID = MsgAudio
	}

	msg := &Message{
		TypeID:    msgTypeID,
		Length:    uint32(len(payload)),
		Timestamp: uint32(frame.DTS),
		StreamID:  1,
		Payload:   payload,
	}

	return s.cw.WriteMessage(6, msg)
}
