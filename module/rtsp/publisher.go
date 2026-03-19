package rtsp

import (
	"sync"

	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	pkgrtp "github.com/im-pingo/liveforge/pkg/rtp"
	pionrtp "github.com/pion/rtp/v2"
)

// RTSPPublisher implements core.Publisher for RTSP ANNOUNCE/RECORD.
type RTSPPublisher struct {
	id            string
	mediaInfo     *avframe.MediaInfo
	stream        *core.Stream
	depacketizers map[uint8]pkgrtp.Depacketizer // payloadType → depacketizer
	mu            sync.Mutex
	closed        bool
}

// Verify interface compliance.
var _ core.Publisher = (*RTSPPublisher)(nil)

// NewRTSPPublisher creates a new RTSP publisher.
func NewRTSPPublisher(id string, info *avframe.MediaInfo, stream *core.Stream) (*RTSPPublisher, error) {
	p := &RTSPPublisher{
		id:            id,
		mediaInfo:     info,
		stream:        stream,
		depacketizers: make(map[uint8]pkgrtp.Depacketizer),
	}
	// Set up depacketizers based on MediaInfo codecs.
	// Use conventional PT assignments matching pkg/sdp/builder.go codecRTPInfo.
	if info.VideoCodec != 0 {
		dp, err := pkgrtp.NewDepacketizer(info.VideoCodec)
		if err != nil {
			return nil, err
		}
		pt := codecDefaultPT(info.VideoCodec)
		p.depacketizers[pt] = dp
	}
	if info.AudioCodec != 0 {
		dp, err := pkgrtp.NewDepacketizer(info.AudioCodec)
		if err != nil {
			return nil, err
		}
		pt := codecDefaultPT(info.AudioCodec)
		p.depacketizers[pt] = dp
	}
	return p, nil
}

func (p *RTSPPublisher) ID() string                    { return p.id }
func (p *RTSPPublisher) MediaInfo() *avframe.MediaInfo { return p.mediaInfo }

func (p *RTSPPublisher) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	return nil
}

// FeedRTP processes an incoming RTP packet.
func (p *RTSPPublisher) FeedRTP(pkt *pionrtp.Packet) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	dp, ok := p.depacketizers[pkt.PayloadType]
	p.mu.Unlock()
	if !ok {
		return nil // Unknown PT, skip
	}
	frame, err := dp.Depacketize(pkt)
	if err != nil {
		return err
	}
	if frame != nil {
		p.stream.WriteFrame(frame)
	}
	return nil
}

// codecDefaultPT returns the default payload type for a codec.
// Must match the PT assignments in pkg/sdp/builder.go codecRTPInfo.
func codecDefaultPT(codec avframe.CodecType) uint8 {
	switch codec {
	case avframe.CodecH264:
		return 96
	case avframe.CodecH265:
		return 97
	case avframe.CodecVP8:
		return 98
	case avframe.CodecVP9:
		return 99
	case avframe.CodecAV1:
		return 100
	case avframe.CodecAAC:
		return 101
	case avframe.CodecOpus:
		return 111
	case avframe.CodecMP3:
		return 14
	case avframe.CodecG711U:
		return 0
	case avframe.CodecG711A:
		return 8
	case avframe.CodecG722:
		return 9
	case avframe.CodecG729:
		return 18
	case avframe.CodecSpeex:
		return 102
	default:
		return 96
	}
}
