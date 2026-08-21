package rtsp

import (
	"io"
	"log/slog"
	"math/rand"
	"sync"
	"time"

	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	pkgrtp "github.com/im-pingo/liveforge/pkg/rtp"
	pionrtp "github.com/pion/rtp/v2"
)

const maxRTPSSRCStatesPerPT = 16

// RTSPPublisher implements core.Publisher for RTSP ANNOUNCE/RECORD.
type RTSPPublisher struct {
	id          string
	mediaInfo   *avframe.MediaInfo
	stream      *core.Stream
	ptMap       RTPTrackMap
	trackMap    map[int]RTPTrackInfo
	trackPT     map[int]uint8
	trackStates map[rtpTrackKey]*rtpTrackState
	trackUseSeq uint64
	mu          sync.Mutex
	closed      bool
	frameCount  int

	// RTCP RR support
	rtcpWriter   io.Writer // TCP conn for sending RTCP
	rtcpChannel  uint8     // RTCP channel (odd, e.g. 1 for video)
	localSSRC    uint32
	receivedPkts uint32
	highestSeq   uint32
	done         chan struct{}
}

type rtpTrackKey struct {
	trackID     int
	payloadType uint8
	ssrc        uint32
}

type rtpTrackState struct {
	mu           sync.Mutex
	depacketizer pkgrtp.Depacketizer
	clockRate    uint32

	accPayload   []byte
	accFrameType avframe.FrameType
	accMediaType avframe.MediaType
	accCodec     avframe.CodecType

	timestampSet  bool
	lastTimestamp uint32
	lastExtended  uint64
	baseSet       bool
	baseExtended  uint64
	lastUsed      uint64 // protected by RTSPPublisher.mu
}

// Verify interface compliance.
var _ core.Publisher = (*RTSPPublisher)(nil)

// NewRTSPPublisher creates a publisher from a legacy payload-type-to-codec map.
func NewRTSPPublisher(id string, info *avframe.MediaInfo, stream *core.Stream, ptMap PTMap) (*RTSPPublisher, error) {
	trackMap := make(RTPTrackMap, len(ptMap))
	for pt, codec := range ptMap {
		trackMap[pt] = RTPTrackInfo{Codec: codec}
	}
	return NewRTSPPublisherWithTracks(id, info, stream, trackMap)
}

// NewRTSPPublisherWithTracks creates a publisher with codec and RTP clock metadata.
// If trackMap is nil, default PT assignments and codec clock rates are used.
func NewRTSPPublisherWithTracks(id string, info *avframe.MediaInfo, stream *core.Stream, trackMap RTPTrackMap) (*RTSPPublisher, error) {
	p := &RTSPPublisher{
		id:          id,
		mediaInfo:   info,
		stream:      stream,
		ptMap:       make(RTPTrackMap),
		trackMap:    make(map[int]RTPTrackInfo),
		trackPT:     make(map[int]uint8),
		trackStates: make(map[rtpTrackKey]*rtpTrackState),
		localSSRC:   rand.Uint32(),
		done:        make(chan struct{}),
	}

	if len(trackMap) > 0 {
		for pt, trackInfo := range trackMap {
			if err := validateTrackInfo(trackInfo); err != nil {
				return nil, err
			}
			if trackInfo.ClockRate == 0 {
				trackInfo.ClockRate = defaultRTPClockRate(trackInfo.Codec, info)
			}
			p.ptMap[pt] = trackInfo
		}
	} else {
		if info.VideoCodec != 0 {
			trackInfo := RTPTrackInfo{
				Codec:     info.VideoCodec,
				ClockRate: defaultRTPClockRate(info.VideoCodec, info),
			}
			if err := validateTrackInfo(trackInfo); err != nil {
				return nil, err
			}
			p.ptMap[codecDefaultPT(info.VideoCodec)] = trackInfo
		}
		if info.AudioCodec != 0 {
			trackInfo := RTPTrackInfo{
				Codec:     info.AudioCodec,
				ClockRate: defaultRTPClockRate(info.AudioCodec, info),
			}
			if err := validateTrackInfo(trackInfo); err != nil {
				return nil, err
			}
			p.ptMap[codecDefaultPT(info.AudioCodec)] = trackInfo
		}
	}

	return p, nil
}

// NewRTSPPublisherWithDescriptions creates a publisher whose RTP state is
// selected by SDP track identity. This is required when multiple media lines
// reuse the same payload type.
func NewRTSPPublisherWithDescriptions(id string, info *avframe.MediaInfo, stream *core.Stream, descriptions []RTPTrackDescription) (*RTSPPublisher, error) {
	p := &RTSPPublisher{
		id:          id,
		mediaInfo:   info,
		stream:      stream,
		ptMap:       make(RTPTrackMap),
		trackMap:    make(map[int]RTPTrackInfo, len(descriptions)),
		trackPT:     make(map[int]uint8, len(descriptions)),
		trackStates: make(map[rtpTrackKey]*rtpTrackState),
		localSSRC:   rand.Uint32(),
		done:        make(chan struct{}),
	}
	for _, description := range descriptions {
		trackInfo := description.Info
		if trackInfo.ClockRate == 0 {
			trackInfo.ClockRate = defaultRTPClockRate(trackInfo.Codec, info)
		}
		if err := validateTrackInfo(trackInfo); err != nil {
			return nil, err
		}
		p.trackMap[description.TrackID] = trackInfo
		p.trackPT[description.TrackID] = description.PayloadType
		// Keep a deterministic PT fallback for legacy callers. A duplicate PT
		// is intentionally not overwritten, because it has no unique identity.
		if _, exists := p.ptMap[description.PayloadType]; !exists {
			p.ptMap[description.PayloadType] = trackInfo
		}
	}
	return p, nil
}

func (p *RTSPPublisher) ID() string                    { return p.id }
func (p *RTSPPublisher) MediaInfo() *avframe.MediaInfo { return p.mediaInfo }

func (p *RTSPPublisher) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.closed {
		p.closed = true
		if p.done != nil {
			close(p.done)
		}
	}
	return nil
}

// SetRTCPWriter configures the writer and channel for sending RTCP RR to the client.
func (p *RTSPPublisher) SetRTCPWriter(w io.Writer, rtcpChannel uint8) {
	p.mu.Lock()
	p.rtcpWriter = w
	p.rtcpChannel = rtcpChannel
	p.mu.Unlock()
	go p.rtcpLoop()
}

// rtcpLoop sends periodic RTCP Receiver Reports to the pushing client.
func (p *RTSPPublisher) rtcpLoop() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			p.mu.Lock()
			if p.closed || p.rtcpWriter == nil {
				p.mu.Unlock()
				return
			}
			w := p.rtcpWriter
			ch := p.rtcpChannel
			received := p.receivedPkts
			highest := p.highestSeq
			p.mu.Unlock()

			rr := pkgrtp.BuildRR(p.localSSRC, []pkgrtp.ReceptionReport{
				{
					SSRC:       0, // remote SSRC (unknown until first packet)
					HighestSeq: highest,
				},
			})
			_ = received // tracked for future loss calculation
			WriteInterleaved(w, ch, rr)
		case <-p.done:
			return
		}
	}
}

// FeedRTP processes an incoming RTP packet.
// NALs are accumulated until the RTP marker bit indicates end of access unit,
// then emitted as a single AVFrame with all NALs in AVCC format.
func (p *RTSPPublisher) FeedRTP(pkt *pionrtp.Packet) error {
	return p.feedRTPForTrack(pkt, -1)
}

// FeedRTPForTrack processes an RTP packet for the SDP track selected during
// SETUP. It avoids payload-type ambiguity when tracks reuse a PT.
func (p *RTSPPublisher) FeedRTPForTrack(pkt *pionrtp.Packet, trackID int) error {
	return p.feedRTPForTrack(pkt, trackID)
}

func (p *RTSPPublisher) feedRTPForTrack(pkt *pionrtp.Packet, trackID int) error {
	track, ok, err := p.trackState(pkt, trackID)
	if err != nil {
		slog.Error("FeedRTP create track state error", "module", "rtsp", "error", err, "track", trackID)
		return err
	}
	if !ok {
		slog.Debug("FeedRTP unknown PT or track", "module", "rtsp", "pt", pkt.PayloadType, "track", trackID, "registered", p.registeredPTs())
		return nil
	}

	track.mu.Lock()
	auFrame, err := track.processLocked(pkt)
	if err == nil && auFrame != nil {
		if p.stream != nil {
			p.stream.WriteFrame(auFrame)
		}
	}
	track.mu.Unlock()
	if err != nil {
		slog.Error("FeedRTP depacketize error", "module", "rtsp", "error", err)
		return err
	}
	if auFrame == nil {
		return nil
	}

	p.mu.Lock()
	p.frameCount++
	p.mu.Unlock()
	return nil
}

func (p *RTSPPublisher) trackState(pkt *pionrtp.Packet, trackID int) (*rtpTrackState, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, false, nil
	}

	p.receivedPkts++
	seq := uint32(pkt.SequenceNumber)
	if seq > p.highestSeq {
		p.highestSeq = seq
	}

	var trackInfo RTPTrackInfo
	var ok bool
	if trackID >= 0 {
		trackInfo, ok = p.trackMap[trackID]
		if !ok || p.trackPT[trackID] != pkt.PayloadType {
			return nil, false, nil
		}
	} else {
		trackInfo, ok = p.ptMap[pkt.PayloadType]
	}
	if !ok {
		return nil, false, nil
	}
	p.trackUseSeq++
	key := rtpTrackKey{trackID: trackID, payloadType: pkt.PayloadType, ssrc: pkt.SSRC}
	if track := p.trackStates[key]; track != nil {
		track.lastUsed = p.trackUseSeq
		return track, true, nil
	}
	p.evictOldestTrackState(trackID, pkt.PayloadType)

	dp, err := pkgrtp.NewDepacketizer(trackInfo.Codec)
	if err != nil {
		return nil, false, err
	}
	track := &rtpTrackState{
		depacketizer: dp,
		clockRate:    trackInfo.ClockRate,
		lastUsed:     p.trackUseSeq,
	}
	p.trackStates[key] = track
	return track, true, nil
}

func (t *rtpTrackState) processLocked(pkt *pionrtp.Packet) (*avframe.AVFrame, error) {
	frame, err := t.depacketizer.Depacketize(pkt)
	if err != nil {
		return nil, err
	}
	if frame != nil {
		t.accPayload = append(t.accPayload, frame.Payload...)
		t.accMediaType = frame.MediaType
		t.accCodec = frame.Codec
		t.accFrameType = bestFrameType(t.accFrameType, frame.FrameType)
	}
	complete := pkt.Marker || (frame != nil && frame.MediaType.IsAudio())
	if !complete || len(t.accPayload) == 0 {
		return nil, nil
	}

	extended := t.unwrapTimestamp(pkt.Timestamp)
	if !t.baseSet {
		t.baseSet = true
		t.baseExtended = extended
	}
	dts := rtpTicksToMilliseconds(int64(extended)-int64(t.baseExtended), t.clockRate)
	auFrame := avframe.NewAVFrame(
		t.accMediaType,
		t.accCodec,
		t.accFrameType,
		dts, dts,
		t.accPayload,
	)
	t.accPayload = nil
	t.accFrameType = 0
	return auFrame, nil
}

// evictOldestTrackState keeps SSRC churn on one payload type from growing
// state for the entire RTSP session. Caller must hold p.mu.
func (p *RTSPPublisher) evictOldestTrackState(trackID int, payloadType uint8) {
	count := 0
	var oldestKey rtpTrackKey
	var oldestUse uint64
	for key, track := range p.trackStates {
		if key.trackID != trackID || key.payloadType != payloadType {
			continue
		}
		count++
		if oldestUse == 0 || track.lastUsed < oldestUse {
			oldestKey = key
			oldestUse = track.lastUsed
		}
	}
	if count >= maxRTPSSRCStatesPerPT {
		delete(p.trackStates, oldestKey)
	}
}

func (t *rtpTrackState) unwrapTimestamp(timestamp uint32) uint64 {
	if !t.timestampSet {
		t.timestampSet = true
		t.lastTimestamp = timestamp
		t.lastExtended = uint64(timestamp)
		return t.lastExtended
	}

	delta := int64(int32(timestamp - t.lastTimestamp))
	if delta > 0 {
		t.lastTimestamp = timestamp
		t.lastExtended += uint64(delta)
		return t.lastExtended
	}
	if delta == 0 {
		return t.lastExtended
	}
	if behind := uint64(-delta); behind <= t.lastExtended {
		return t.lastExtended - behind
	}
	return 0
}

func bestFrameType(current, next avframe.FrameType) avframe.FrameType {
	if next == avframe.FrameTypeSequenceHeader {
		return avframe.FrameTypeSequenceHeader
	}
	if next == avframe.FrameTypeKeyframe && current != avframe.FrameTypeSequenceHeader {
		return avframe.FrameTypeKeyframe
	}
	if current == 0 {
		return next
	}
	return current
}

func rtpTicksToMilliseconds(ticks int64, clockRate uint32) int64 {
	if clockRate == 0 {
		return 0
	}
	return ticks * 1000 / int64(clockRate)
}

func (p *RTSPPublisher) registeredPTs() []uint8 {
	p.mu.Lock()
	defer p.mu.Unlock()
	pts := make([]uint8, 0, len(p.ptMap))
	for pt := range p.ptMap {
		pts = append(pts, pt)
	}
	return pts
}

func validateTrackInfo(trackInfo RTPTrackInfo) error {
	_, err := pkgrtp.NewDepacketizer(trackInfo.Codec)
	return err
}

func defaultRTPClockRate(codec avframe.CodecType, info *avframe.MediaInfo) uint32 {
	switch codec {
	case avframe.CodecH264, avframe.CodecH265, avframe.CodecAV1, avframe.CodecVP8, avframe.CodecVP9, avframe.CodecMP3:
		return 90000
	case avframe.CodecOpus:
		return 48000
	case avframe.CodecG711A, avframe.CodecG711U, avframe.CodecG722, avframe.CodecG729:
		return 8000
	case avframe.CodecAAC:
		if info != nil && info.SampleRate > 0 {
			return uint32(info.SampleRate)
		}
		return 44100
	case avframe.CodecSpeex:
		if info != nil && info.SampleRate > 0 {
			return uint32(info.SampleRate)
		}
		return 8000
	default:
		return 90000
	}
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
