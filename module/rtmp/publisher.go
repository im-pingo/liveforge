package rtmp

import (
	"net"

	"github.com/im-pingo/liveforge/pkg/avframe"
)

// Publisher implements core.Publisher for RTMP connections.
type Publisher struct {
	id   string
	conn net.Conn
	info *avframe.MediaInfo
}

// NewPublisher creates a new RTMP publisher.
func NewPublisher(streamKey string, conn net.Conn) *Publisher {
	return &Publisher{
		id:   "rtmp-pub-" + streamKey,
		conn: conn,
		info: &avframe.MediaInfo{},
	}
}

// ID returns the publisher identifier.
func (p *Publisher) ID() string { return p.id }

// MediaInfo returns the codec information for this publisher.
func (p *Publisher) MediaInfo() *avframe.MediaInfo { return p.info }

// Close disconnects the publisher.
func (p *Publisher) Close() error { return p.conn.Close() }

// SetMediaInfo updates the media info when sequence headers are received.
func (p *Publisher) SetMediaInfo(info *avframe.MediaInfo) {
	p.info = info
}
