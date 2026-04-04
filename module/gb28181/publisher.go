package gb28181

import (
	"log/slog"
	"sync"

	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/muxer/ps"
	pionrtp "github.com/pion/rtp/v2"
)

// Publisher receives RTP/PS packets and writes AVFrames to a stream.
type Publisher struct {
	mu       sync.Mutex
	id       string
	info     *avframe.MediaInfo
	demuxer  *ps.Demuxer
	callback func(*avframe.AVFrame)
	closed   bool
	psBuf    []byte // accumulation buffer for fragmented PS packs
}

// NewPublisher creates a new GB28181 publisher.
func NewPublisher(id string, callback func(*avframe.AVFrame)) *Publisher {
	return &Publisher{
		id:       id,
		demuxer:  ps.NewDemuxer(),
		callback: callback,
		info:     &avframe.MediaInfo{},
	}
}

// ID returns the publisher identifier.
func (p *Publisher) ID() string { return p.id }

// MediaInfo returns the detected media info.
func (p *Publisher) MediaInfo() *avframe.MediaInfo {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Update from demuxer's detected codecs
	if vc := p.demuxer.VideoCodec(); vc != 0 {
		p.info.VideoCodec = vc
	}
	if ac := p.demuxer.AudioCodec(); ac != 0 {
		p.info.AudioCodec = ac
	}
	return p.info
}

// FeedRTP processes an incoming RTP packet containing PS data.
func (p *Publisher) FeedRTP(pkt *pionrtp.Packet) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return
	}

	// Accumulate RTP payload into PS buffer
	p.psBuf = append(p.psBuf, pkt.Payload...)

	// If marker bit is set, we have a complete PS pack / access unit
	if pkt.Marker {
		p.processPSPack()
	}
}

// processPSPack demuxes the accumulated PS buffer and emits AVFrames.
func (p *Publisher) processPSPack() {
	if len(p.psBuf) == 0 {
		return
	}

	frames, err := p.demuxer.Feed(p.psBuf)
	if err != nil {
		slog.Debug("ps demux error", "module", "gb28181", "publisher", p.id, "error", err)
	}

	for _, frame := range frames {
		if p.callback != nil {
			// Copy payload: Feed returns slices into p.psBuf which
			// will be overwritten when the next PS pack arrives.
			payload := make([]byte, len(frame.Payload))
			copy(payload, frame.Payload)
			frame.Payload = payload
			p.callback(frame)
		}
	}

	p.psBuf = p.psBuf[:0]
}

// Close stops the publisher.
func (p *Publisher) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	return nil
}
