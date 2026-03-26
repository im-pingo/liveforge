package srt

import (
	"log"

	gosrt "github.com/datarhei/gosrt"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
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
		log.Printf("SRT publisher %s: get stream error: %v", p.streamKey, err)
		return
	}

	if err := stream.SetPublisher(p); err != nil {
		log.Printf("SRT publisher %s: set publisher error: %v", p.streamKey, err)
		return
	}

	defer func() {
		stream.RemovePublisher()
		p.eventBus.Emit(core.EventPublishStop, &core.EventContext{ //nolint:errcheck
			StreamKey: p.streamKey,
			Protocol:  "srt",
		})
	}()

	// Read MPEG-TS data from SRT connection.
	// SRT delivers data in message-mode chunks (typically 1316 bytes = 7 TS packets).
	buf := make([]byte, 1500)
	for {
		n, err := p.conn.Read(buf)
		if err != nil {
			return
		}
		if n == 0 {
			continue
		}

		// TODO: Feed buf[:n] into a TS demuxer to extract PES packets,
		// decode them into AVFrames, and write to stream via stream.WriteFrame().
		// This requires implementing a TS demuxer in pkg/muxer/ts/demuxer.go.
		// For now, the raw TS data is received but not yet parsed.
		_ = buf[:n]
	}
}
