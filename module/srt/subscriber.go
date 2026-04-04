package srt

import (
	"io"
	"log/slog"
	"time"

	gosrt "github.com/datarhei/gosrt"
	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/muxer/ts"
)

// Subscriber reads AVFrames from a stream and muxes them into MPEG-TS,
// writing the result to an SRT connection.
type Subscriber struct {
	conn      gosrt.Conn
	streamKey string
	hub       *core.StreamHub
	eventBus  *core.EventBus
	skipCfg   *config.SkipTrackerConfig
	closed    chan struct{}
}

// NewSubscriber creates a new SRT subscriber.
func NewSubscriber(conn gosrt.Conn, streamKey string, hub *core.StreamHub, bus *core.EventBus, skipCfg *config.SkipTrackerConfig) *Subscriber {
	return &Subscriber{
		conn:      conn,
		streamKey: streamKey,
		hub:       hub,
		eventBus:  bus,
		skipCfg:   skipCfg,
		closed:    make(chan struct{}),
	}
}

// Run reads frames from the stream ring buffer, muxes them to MPEG-TS,
// and writes the result to the SRT connection. It blocks until the connection
// is closed or the stream ends.
func (s *Subscriber) Run() {
	stream, ok := s.hub.Find(s.streamKey)
	if !ok {
		slog.Warn("stream not found", "module", "srt", "stream", s.streamKey)
		return
	}

	if err := stream.AddSubscriber("srt"); err != nil {
		slog.Error("add subscriber error", "module", "srt", "stream", s.streamKey, "error", err)
		return
	}
	defer stream.RemoveSubscriber("srt")

	s.eventBus.Emit(core.EventSubscribe, &core.EventContext{ //nolint:errcheck
		StreamKey:  s.streamKey,
		Protocol:   "srt",
		RemoteAddr: s.conn.RemoteAddr().String(),
	})
	defer s.eventBus.Emit(core.EventSubscribeStop, &core.EventContext{ //nolint:errcheck
		StreamKey: s.streamKey,
		Protocol:  "srt",
	})

	// Wait for sequence headers to initialize the TS muxer.
	if !s.waitForSequenceHeaders(stream) {
		return
	}

	// Build TS muxer from publisher's codec info.
	pub := stream.Publisher()
	if pub == nil {
		return
	}

	mi := pub.MediaInfo()
	var videoSeqData, audioSeqData []byte
	if vsh := stream.VideoSeqHeader(); vsh != nil {
		videoSeqData = vsh.Payload
	}
	if ash := stream.AudioSeqHeader(); ash != nil {
		audioSeqData = ash.Payload
	}

	muxer := ts.NewMuxer(mi.VideoCodec, mi.AudioCodec, videoSeqData, audioSeqData)

	// Send GOP cache first for fast startup; track the highest DTS sent
	// so we can skip duplicate frames from the ring buffer.
	var lastDTS int64
	for _, frame := range stream.GOPCache() {
		if err := s.sendFrame(muxer, frame); err != nil {
			return
		}
		if frame.DTS > lastDTS {
			lastDTS = frame.DTS
		}
	}

	// Start the ring buffer reader from the current write position to avoid
	// reading the entire backlog. Combined with the DTS filter below, this
	// prevents backward DTS jumps while tolerating small overlaps.
	reader := stream.RingBuffer().NewReaderAt(stream.RingBuffer().WriteCursor())
	filter := core.NewSlowConsumerFilter(reader, stream.Config().SlowConsumer, s.skipCfg)
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

		if frame.FrameType == avframe.FrameTypeSequenceHeader {
			continue
		}

		// Skip frames already covered by the GOP cache
		if frame.DTS <= lastDTS {
			continue
		}

		start := time.Now()
		if err := s.sendFrame(muxer, frame); err != nil {
			return
		}
		filter.ReportSendTime(time.Since(start))
	}
}

// sendFrame muxes a single AVFrame into TS packets and writes them to the SRT
// connection. If the SRT write queue is full (which returns io.EOF), the write
// is retried after a short delay to allow the congestion control to drain the
// queue. This is essential for burst writes like the GOP cache.
func (s *Subscriber) sendFrame(muxer *ts.Muxer, frame *avframe.AVFrame) error {
	data := muxer.WriteFrame(frame)
	if len(data) == 0 {
		return nil
	}

	const maxRetries = 50
	for i := 0; i < maxRetries; i++ {
		_, err := s.conn.Write(data)
		if err == nil {
			return nil
		}
		// The gosrt library returns io.EOF when the internal write queue is
		// full (non-blocking channel send). Retry after a brief pause.
		if err == io.EOF {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		return err
	}
	return io.EOF
}

// waitForSequenceHeaders blocks until at least one sequence header is available.
func (s *Subscriber) waitForSequenceHeaders(stream *core.Stream) bool {
	for {
		if stream.VideoSeqHeader() != nil || stream.AudioSeqHeader() != nil {
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
