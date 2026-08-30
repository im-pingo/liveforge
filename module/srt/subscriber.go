package srt

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"
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
	id        string
	skipCfg   *config.SkipTrackerConfig
	closed    chan struct{}
}

var subscriberSequence atomic.Uint64

// NewSubscriber creates a new SRT subscriber.
func NewSubscriber(conn gosrt.Conn, streamKey string, hub *core.StreamHub, bus *core.EventBus, skipCfg *config.SkipTrackerConfig) *Subscriber {
	generation := subscriberSequence.Add(1)
	id := fmt.Sprintf("srt-sub-%s-local-%d", streamKey, generation)
	if conn != nil {
		id = fmt.Sprintf("srt-sub-%s-%d-%d-%d", streamKey, conn.SocketId(), conn.PeerSocketId(), generation)
	}
	return &Subscriber{
		conn:      conn,
		streamKey: streamKey,
		hub:       hub,
		eventBus:  bus,
		id:        id,
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

	pending := stream.StartupSnapshot()
	releaseSubscriber, err := stream.AddSubscriberForGeneration("srt", pending.Generation)
	if err != nil {
		slog.Error("add subscriber error", "module", "srt", "stream", s.streamKey, "error", err)
		return
	}
	defer releaseSubscriber()

	lifecycleCtx := &core.EventContext{
		StreamKey:           s.streamKey,
		StreamInstanceID:    pending.StreamInstanceID,
		PublisherGeneration: pending.Generation,
		PublisherID:         pending.PublisherID,
		SubscriberID:        s.id,
		Protocol:            "srt",
		RemoteAddr:          s.conn.RemoteAddr().String(),
	}
	if err := s.eventBus.EmitAsync(core.EventSubscribe, lifecycleCtx); err != nil {
		slog.Error("subscriber lifecycle admission failed", "module", "srt", "stream", s.streamKey, "error", err)
		return
	}
	defer func() {
		if err := s.eventBus.EmitAsync(core.EventSubscribeStop, lifecycleCtx); err != nil {
			slog.Error("subscriber terminal lifecycle admission failed", "module", "srt", "stream", s.streamKey, "error", err)
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-s.closed:
		case <-pending.GenerationDone:
			cancel()
		case <-ctx.Done():
		}
	}()
	snapshot := pending
	if !snapshot.Ready {
		var ok bool
		snapshot, ok = stream.WaitForStartup(ctx)
		if !ok || snapshot.Generation != pending.Generation {
			return
		}
	}

	videoCodec := snapshot.MediaInfo.VideoCodec
	audioCodec := snapshot.MediaInfo.AudioCodec
	var videoSeqData, audioSeqData []byte
	if vsh := snapshot.VideoSequenceHeader; vsh != nil {
		videoSeqData = vsh.Payload
	}
	if ash := snapshot.AudioSequenceHeader; ash != nil {
		audioSeqData = ash.Payload
	}
	muxer := ts.NewMuxer(videoCodec, audioCodec, videoSeqData, audioSeqData)

	for _, frame := range snapshot.ReplayFrames {
		if !stream.IsPublisherGeneration(snapshot.Generation) {
			return
		}
		if err := s.sendFrame(muxer, frame); err != nil {
			return
		}
	}

	reader := stream.RingBuffer().NewReaderAt(snapshot.LiveCursor)
	filter := core.NewSlowConsumerFilter(reader, stream.Config().SlowConsumer, s.skipCfg)

	// Watch for subscriber close and unblock any in-progress Read().
	go func() {
		select {
		case <-s.closed:
		case <-snapshot.GenerationDone:
		}
		filter.Close()
	}()

	for {
		result := filter.NextFrameResult()
		if result.Overwritten > 0 {
			reader.AdvanceToLive()
			slog.Warn("srt subscriber source reader overwritten", "module", "srt", "stream", s.streamKey, "overwritten", result.Overwritten)
			return
		}
		if !result.OK {
			return
		}
		frame := result.Frame
		if frame == nil {
			continue
		}
		if !stream.IsPublisherGeneration(snapshot.Generation) {
			return
		}
		if frame.FrameType == avframe.FrameTypeSequenceHeader {
			if frame.MediaType.IsVideo() {
				videoCodec = frame.Codec
				videoSeqData = frame.Payload
			} else if frame.MediaType.IsAudio() {
				audioCodec = frame.Codec
				audioSeqData = frame.Payload
			}
			muxer = ts.NewMuxer(videoCodec, audioCodec, videoSeqData, audioSeqData)
			if err := s.sendData(muxer.WritePATAndPMT()); err != nil {
				return
			}
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
	return s.sendData(data)
}

func (s *Subscriber) sendData(data []byte) error {
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
