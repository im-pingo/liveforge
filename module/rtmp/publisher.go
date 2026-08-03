package rtmp

import (
	"net"
	"sync/atomic"

	"github.com/im-pingo/liveforge/pkg/avframe"
)

// Publisher implements core.Publisher for RTMP connections.
//
// MediaInfo is stored behind an atomic pointer: the handler goroutine
// publishes updated snapshots via SetMediaInfo while subscriber goroutines
// (RTSP, WebRTC, HTTP muxers) read concurrently. Snapshots are never
// mutated after publication.
type Publisher struct {
	id   string
	conn net.Conn
	info atomic.Pointer[avframe.MediaInfo]
}

// NewPublisher creates a new RTMP publisher.
func NewPublisher(streamKey string, conn net.Conn) *Publisher {
	p := &Publisher{
		id:   "rtmp-pub-" + streamKey,
		conn: conn,
	}
	p.info.Store(&avframe.MediaInfo{})
	return p
}

// ID returns the publisher identifier.
func (p *Publisher) ID() string { return p.id }

// MediaInfo returns the current codec information snapshot.
// The returned struct must not be modified.
func (p *Publisher) MediaInfo() *avframe.MediaInfo { return p.info.Load() }

// Close disconnects the publisher.
func (p *Publisher) Close() error { return p.conn.Close() }

// SetMediaInfo atomically publishes a new media info snapshot.
func (p *Publisher) SetMediaInfo(info *avframe.MediaInfo) {
	p.info.Store(info)
}
