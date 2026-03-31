package srt

import (
	"log/slog"

	gosrt "github.com/datarhei/gosrt"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/muxer/ts"
)

// Publisher reads MPEG-TS data from an SRT connection and feeds AVFrames
// into the StreamHub.
type Publisher struct {
	conn      gosrt.Conn
	streamKey string
	hub       *core.StreamHub
	eventBus  *core.EventBus
	info      *avframe.MediaInfo
}

// NewPublisher creates a new SRT publisher.
func NewPublisher(conn gosrt.Conn, streamKey string, hub *core.StreamHub, bus *core.EventBus) *Publisher {
	return &Publisher{
		conn:      conn,
		streamKey: streamKey,
		hub:       hub,
		eventBus:  bus,
		info:      &avframe.MediaInfo{},
	}
}

// ID returns the publisher identifier.
func (p *Publisher) ID() string { return "srt-pub-" + p.streamKey }

// MediaInfo returns the codec information for this publisher.
func (p *Publisher) MediaInfo() *avframe.MediaInfo { return p.info }

// Close disconnects the publisher.
func (p *Publisher) Close() error { return p.conn.Close() }

// Run reads MPEG-TS packets from the SRT connection and demuxes them
// into AVFrames written to the stream. It blocks until the connection
// is closed or an error occurs.
func (p *Publisher) Run() {
	stream, err := p.hub.GetOrCreate(p.streamKey)
	if err != nil {
		slog.Error("get stream error", "module", "srt", "stream", p.streamKey, "error", err)
		return
	}

	if err := stream.SetPublisher(p); err != nil {
		slog.Error("set publisher error", "module", "srt", "stream", p.streamKey, "error", err)
		return
	}

	defer func() {
		stream.RemovePublisher()
		p.eventBus.Emit(core.EventPublishStop, &core.EventContext{ //nolint:errcheck
			StreamKey: p.streamKey,
			Protocol:  "srt",
		})
	}()

	// Demux MPEG-TS data from SRT connection into AVFrames.
	demuxer := ts.NewDemuxer(func(frame *avframe.AVFrame) {
		if frame.FrameType == avframe.FrameTypeSequenceHeader {
			if frame.MediaType.IsVideo() {
				p.info.VideoCodec = frame.Codec
				p.info.VideoSequenceHeader = frame.Payload
			} else if frame.MediaType.IsAudio() {
				p.info.AudioCodec = frame.Codec
				p.info.AudioSequenceHeader = frame.Payload
			}
		}
		stream.WriteFrame(frame)
	})

	// SRT delivers data in message-mode chunks (typically 1316 bytes = 7 TS packets).
	buf := make([]byte, 1500)
	for {
		n, err := p.conn.Read(buf)
		if err != nil {
			break
		}
		if n > 0 {
			demuxer.Feed(buf[:n])
		}
	}
	demuxer.Flush()
}
