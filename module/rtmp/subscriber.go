package rtmp

import (
	"bytes"
	"log/slog"
	"net"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	flvpkg "github.com/im-pingo/liveforge/pkg/muxer/flv"
	"github.com/im-pingo/liveforge/pkg/util"
)

// Subscriber implements core.Subscriber for RTMP connections.
type Subscriber struct {
	id      string
	conn    net.Conn
	cw      *ChunkWriter
	stream  *core.Stream
	opts    core.SubscribeOptions
	skipCfg *config.SkipTrackerConfig
	closed  chan struct{}

	// Reusable per-frame encoding state to avoid heap allocations on the hot path.
	flvBuf bytes.Buffer
	muxer  *flvpkg.Muxer
}

// NewSubscriber creates a new RTMP subscriber.
func NewSubscriber(streamKey string, conn net.Conn, cw *ChunkWriter, stream *core.Stream, skipCfg *config.SkipTrackerConfig) *Subscriber {
	return &Subscriber{
		id:      "rtmp-sub-" + streamKey,
		conn:    conn,
		cw:      cw,
		stream:  stream,
		opts:    core.DefaultSubscribeOptions(),
		skipCfg: skipCfg,
		closed:  make(chan struct{}),
		muxer:   flvpkg.NewMuxer(),
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

	// Wait for video sequence header (SPS/PPS) — required for decoder init.
	// If publisher hasn't started yet, poll until available or closed.
	if !s.waitForSequenceHeaders() {
		return
	}

	// Determine if audio transcoding is needed.
	// RTMP/FLV supports AAC and MP3; other codecs (Opus, G.711, etc.) need transcoding.
	var transcodeRelease func()
	needsTranscode := false
	if pub := s.stream.Publisher(); pub != nil {
		ac := pub.MediaInfo().AudioCodec
		if ac != 0 && ac != avframe.CodecAAC && ac != avframe.CodecMP3 {
			needsTranscode = true
		}
	}

	// Send sequence headers
	if vsh := s.stream.VideoSeqHeader(); vsh != nil {
		if err := s.sendFrame(vsh); err != nil {
			slog.Error("video seq header send error", "module", "rtmp", "subscriber", s.id, "error", err)
			return
		}
	}
	if !needsTranscode {
		if ash := s.stream.AudioSeqHeader(); ash != nil {
			if err := s.sendFrame(ash); err != nil {
				slog.Error("audio seq header send error", "module", "rtmp", "subscriber", s.id, "error", err)
				return
			}
		}
	}

	// Snapshot write cursor before sending GOP cache, so the live reader
	// starts right after the cached frames and avoids duplicate/stale data.
	startPos := s.stream.RingBuffer().WriteCursor()

	// Send GOP cache if in GOP mode
	if s.opts.StartMode == core.StartModeGOP {
		for _, frame := range s.stream.GOPCache() {
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

	// Set up the live reader. When transcoding, use TranscodeManager's reader
	// which provides video passthrough and transcoded audio in AAC.
	var reader *util.RingReader[*avframe.AVFrame]
	if needsTranscode {
		if tm := s.stream.TranscodeManager(); tm != nil {
			var err error
			reader, transcodeRelease, err = tm.GetOrCreateReader(avframe.CodecAAC)
			if err != nil {
				slog.Warn("rtmp: audio transcode unavailable", "subscriber", s.id, "error", err)
				reader = s.stream.RingBuffer().NewReaderAt(startPos)
			}
		} else {
			reader = s.stream.RingBuffer().NewReaderAt(startPos)
		}
	} else {
		reader = s.stream.RingBuffer().NewReaderAt(startPos)
	}
	if transcodeRelease != nil {
		defer transcodeRelease()
	}

	filter := core.NewSlowConsumerFilter(reader, s.stream.Config().SlowConsumer, s.skipCfg)

	// Watch for subscriber close and unblock any in-progress Read().
	go func() {
		<-s.closed
		filter.Close()
	}()

	for {
		frame, ok := filter.NextFrame()
		if !ok {
			return
		}

		// Skip sequence headers (already sent during init)
		if frame.FrameType == avframe.FrameTypeSequenceHeader {
			continue
		}

		start := time.Now()
		if err := s.sendFrame(frame); err != nil {
			return
		}
		filter.ReportSendTime(time.Since(start))
	}
}

// waitForSequenceHeaders blocks until at least one sequence header is available,
// or returns false if the subscriber is closed while waiting.
func (s *Subscriber) waitForSequenceHeaders() bool {
	// Fast path: already available
	if s.stream.VideoSeqHeader() != nil || s.stream.AudioSeqHeader() != nil {
		return true
	}
	select {
	case <-s.stream.SeqHeaderReady():
		return true
	case <-s.closed:
		return false
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
