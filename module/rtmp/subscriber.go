package rtmp

import (
	"bytes"
	"log/slog"
	"net"
	"time"

	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	flvpkg "github.com/im-pingo/liveforge/pkg/muxer/flv"
)

// Subscriber implements core.Subscriber for RTMP connections.
type Subscriber struct {
	id      string
	conn    net.Conn
	cw      *ChunkWriter
	stream  *core.Stream
	opts    core.SubscribeOptions
	closed  chan struct{}
}

// NewSubscriber creates a new RTMP subscriber.
func NewSubscriber(streamKey string, conn net.Conn, cw *ChunkWriter, stream *core.Stream) *Subscriber {
	return &Subscriber{
		id:     "rtmp-sub-" + streamKey,
		conn:   conn,
		cw:     cw,
		stream: stream,
		opts:   core.DefaultSubscribeOptions(),
		closed: make(chan struct{}),
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

	// Send sequence headers
	if vsh := s.stream.VideoSeqHeader(); vsh != nil {
		if err := s.sendFrame(vsh); err != nil {
			slog.Error("video seq header send error", "module", "rtmp", "subscriber", s.id, "error", err)
			return
		}
	}
	if ash := s.stream.AudioSeqHeader(); ash != nil {
		if err := s.sendFrame(ash); err != nil {
			slog.Error("audio seq header send error", "module", "rtmp", "subscriber", s.id, "error", err)
			return
		}
	}

	// Send GOP cache if in GOP mode and track last DTS sent
	var lastDTS int64
	if s.opts.StartMode == core.StartModeGOP {
		for _, frame := range s.stream.GOPCache() {
			if err := s.sendFrame(frame); err != nil {
				slog.Error("GOP cache send error", "module", "rtmp", "subscriber", s.id, "error", err)
				return
			}
			if frame.DTS > lastDTS {
				lastDTS = frame.DTS
			}
		}
	}

	// Read live frames from ring buffer, skipping frames already sent via GOP cache
	reader := s.stream.RingBuffer().NewReader()
	filter := core.NewSlowConsumerFilter(reader, s.stream.Config().SlowConsumer)
	for {
		select {
		case <-s.closed:
			return
		default:
		}

		frame, ok := filter.NextFrame()
		if !ok {
			return
		}

		// Skip frames that were already covered by the GOP cache
		if frame.FrameType == avframe.FrameTypeSequenceHeader || frame.DTS <= lastDTS {
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
	for {
		if s.stream.VideoSeqHeader() != nil || s.stream.AudioSeqHeader() != nil {
			return true
		}
		select {
		case <-s.closed:
			return false
		default:
			time.Sleep(50 * time.Millisecond)
		}
	}
}

func (s *Subscriber) buildRTMPPayload(frame *avframe.AVFrame) ([]byte, error) {
	var buf bytes.Buffer
	muxer := flvpkg.NewMuxer()
	if err := muxer.WriteFrame(&buf, frame); err != nil {
		return nil, err
	}

	// Extract the FLV tag body (skip tag header and trailing prev tag size)
	tagData := buf.Bytes()
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
