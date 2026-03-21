package rtsp

import (
	"log"
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
	frameCount    int
	tsBase        uint32 // first RTP timestamp for normalization
	tsBaseSet     bool

	// Access unit accumulation: collect NALs until RTP marker bit
	accPayload   []byte           // accumulated AVCC payload for current access unit
	accFrameType avframe.FrameType // best frame type seen in current access unit
	accMediaType avframe.MediaType
	accCodec     avframe.CodecType
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
// NALs are accumulated until the RTP marker bit indicates end of access unit,
// then emitted as a single AVFrame with all NALs in AVCC format.
func (p *RTSPPublisher) FeedRTP(pkt *pionrtp.Packet) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	dp, ok := p.depacketizers[pkt.PayloadType]
	p.mu.Unlock()
	if !ok {
		log.Printf("rtsp: FeedRTP unknown PT=%d, registered PTs: %v", pkt.PayloadType, p.registeredPTs())
		return nil // Unknown PT, skip
	}
	frame, err := dp.Depacketize(pkt)
	if err != nil {
		log.Printf("rtsp: FeedRTP depacketize error: %v", err)
		return err
	}
	if frame != nil {
		// Accumulate NAL data for the current access unit.
		p.accPayload = append(p.accPayload, frame.Payload...)
		p.accMediaType = frame.MediaType
		p.accCodec = frame.Codec
		// Track best frame type: SequenceHeader > Keyframe > Interframe
		if frame.FrameType == avframe.FrameTypeSequenceHeader {
			p.accFrameType = avframe.FrameTypeSequenceHeader
		} else if frame.FrameType == avframe.FrameTypeKeyframe && p.accFrameType != avframe.FrameTypeSequenceHeader {
			p.accFrameType = avframe.FrameTypeKeyframe
		} else if p.accFrameType == 0 {
			p.accFrameType = frame.FrameType
		}
	}

	// Emit accumulated access unit when marker bit is set (end of frame)
	if pkt.Marker && len(p.accPayload) > 0 {
		if !p.tsBaseSet {
			p.tsBase = pkt.Timestamp
			p.tsBaseSet = true
		}
		dts := int64(pkt.Timestamp-p.tsBase) / 90 // 90kHz → ms
		auFrame := avframe.NewAVFrame(
			p.accMediaType,
			p.accCodec,
			p.accFrameType,
			dts, dts,
			p.accPayload,
		)
		p.frameCount++
		p.stream.WriteFrame(auFrame)

		// Reset accumulation buffer
		p.accPayload = nil
		p.accFrameType = 0
	}

	return nil
}

func (p *RTSPPublisher) registeredPTs() []uint8 {
	pts := make([]uint8, 0, len(p.depacketizers))
	for pt := range p.depacketizers {
		pts = append(pts, pt)
	}
	return pts
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
