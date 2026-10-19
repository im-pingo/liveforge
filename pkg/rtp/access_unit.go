package rtp

import (
	"fmt"

	"github.com/im-pingo/liveforge/pkg/avframe"
	pionrtp "github.com/pion/rtp/v2"
)

const (
	// MaxAccessUnitBytes includes AVCC length prefixes for H.264/H.265.
	MaxAccessUnitBytes   = (16 << 20) + 4
	MaxAccessUnitPackets = 16384
)

// AccessUnitAssembler combines video depacketizer output through the RTP
// marker. It is owned by one serialized receive loop, not shared concurrently.
type AccessUnitAssembler struct {
	codec                avframe.CodecType
	dp                   Depacketizer
	payload              []byte
	frameType            avframe.FrameType
	timestamp            uint32
	ssrc                 uint32
	payloadType          uint8
	sequence             uint16
	haveUnit             bool
	open                 bool
	damaged              bool
	packets              int
	codecBytes           int
	discardedTimestamp   uint32
	discardedSSRC        uint32
	discardedPayloadType uint8
	haveDiscarded        bool
}

// NewAccessUnitAssembler wraps the track's existing video depacketizer.
// Audio packets must continue through their ordinary per-packet path.
func NewAccessUnitAssembler(codec avframe.CodecType, dp Depacketizer) *AccessUnitAssembler {
	return &AccessUnitAssembler{codec: codec, dp: dp}
}

// Discard releases partial media and codec fragments. Remaining packets with
// the damaged timestamp/source/payload type are suppressed, including Marker.
func (a *AccessUnitAssembler) Discard() {
	if a.haveUnit {
		a.discardedTimestamp, a.discardedSSRC, a.discardedPayloadType = a.timestamp, a.ssrc, a.payloadType
		a.haveDiscarded = true
	}
	a.payload = nil
	a.frameType = 0
	a.packets = 0
	a.codecBytes = 0
	a.damaged = true
	// Callers may retain the original depacketizer in a track table. Clear its
	// partial buffers before replacing our reference so those bytes are freed.
	switch dp := a.dp.(type) {
	case *H264Depacketizer:
		dp.resetFU()
	case *H265Depacketizer:
		dp.resetFU()
	case *VP8Depacketizer:
		dp.reset()
	case *VP9Depacketizer:
		dp.buf = nil
	case *AV1Depacketizer:
		dp.buf = nil
	}
	a.dp, _ = NewDepacketizer(a.codec)
}

// Push returns valid sequence headers before any associated media. Media is
// emitted only after an intact access unit's actual RTP Marker is observed.
func (a *AccessUnitAssembler) Push(pkt *pionrtp.Packet) ([]*avframe.AVFrame, error) {
	if pkt == nil || !a.codec.IsVideo() || a.dp == nil {
		a.Discard()
		return nil, fmt.Errorf("RTP access unit requires a video packet and depacketizer")
	}
	sameSource := a.haveUnit && pkt.SSRC == a.ssrc && pkt.PayloadType == a.payloadType
	sameUnit := sameSource && pkt.Timestamp == a.timestamp
	if a.haveDiscarded && pkt.Timestamp == a.discardedTimestamp && pkt.SSRC == a.discardedSSRC && pkt.PayloadType == a.discardedPayloadType {
		return nil, fmt.Errorf("RTP access unit was discarded")
	}
	forwardTimestamp := sameSource && int32(pkt.Timestamp-a.timestamp) > 0
	sequenceRestart := sameSource && int16(pkt.SequenceNumber-a.sequence) <= 0
	if sequenceRestart && !forwardTimestamp {
		if sameUnit {
			a.Discard()
		}
		return nil, fmt.Errorf("RTP access unit received a duplicate or reordered packet")
	}
	if !sameUnit {
		// A timestamp change abandons an unfinished unit; it is never an implicit
		// flush. Completed units retain the codec's cached parameter sets.
		if a.haveUnit && (a.open || a.damaged || !sameSource || sequenceRestart) {
			a.Discard()
		}
		a.payload, a.frameType = nil, 0
		a.packets, a.codecBytes = 0, 0
		a.timestamp, a.ssrc, a.payloadType = pkt.Timestamp, pkt.SSRC, pkt.PayloadType
		a.haveUnit, a.damaged = true, false
	} else if pkt.SequenceNumber != a.sequence+1 {
		a.sequence = pkt.SequenceNumber
		a.Discard()
		return nil, fmt.Errorf("RTP access unit has a sequence gap")
	}
	a.sequence = pkt.SequenceNumber
	if a.damaged {
		return nil, fmt.Errorf("RTP access unit was discarded")
	}
	if a.packets >= MaxAccessUnitPackets || len(pkt.Payload) > MaxAccessUnitBytes {
		a.Discard()
		return nil, fmt.Errorf("RTP access unit exceeds input bounds")
	}
	a.packets++
	a.open = true
	// AV1 and VP9 currently retain their one-byte-descriptor payloads inside
	// the depacketizer until completion. Bound those bytes before decoding.
	// H.264, H.265 and VP8 enforce their own fragmented-NAL/frame byte limits.
	if a.codec == avframe.CodecAV1 || a.codec == avframe.CodecVP9 {
		bytes := max(0, len(pkt.Payload)-1)
		if bytes > MaxAccessUnitBytes-a.codecBytes {
			a.Discard()
			return nil, fmt.Errorf("RTP access unit exceeds %d codec bytes", MaxAccessUnitBytes)
		}
		a.codecBytes += bytes
	}
	if a.codecPending() && !a.isContinuation(pkt.Payload) {
		a.Discard()
		return nil, fmt.Errorf("RTP access unit abandoned an incomplete codec fragment")
	}
	frames, err := DepacketizeFrames(a.dp, pkt)
	if err != nil {
		a.Discard()
		return nil, err
	}
	a.boundCodecCapacity()
	if pkt.Marker && a.codecPending() {
		a.Discard()
		return nil, fmt.Errorf("RTP marker precedes codec fragment completion")
	}
	// Measure the complete batch before retaining or emitting any of it.
	size := len(a.payload)
	for _, frame := range frames {
		if frame == nil {
			continue
		}
		if len(frame.Payload) > MaxAccessUnitBytes || (frame.FrameType != avframe.FrameTypeSequenceHeader && len(frame.Payload) > MaxAccessUnitBytes-size) {
			a.Discard()
			return nil, fmt.Errorf("RTP access unit exceeds %d output bytes", MaxAccessUnitBytes)
		}
		if frame.FrameType != avframe.FrameTypeSequenceHeader {
			size += len(frame.Payload)
		}
	}
	var output []*avframe.AVFrame
	for _, frame := range frames {
		if frame == nil {
			continue
		}
		if frame.FrameType == avframe.FrameTypeSequenceHeader {
			output = append(output, frame)
			continue
		}
		if needed := len(a.payload) + len(frame.Payload); needed > cap(a.payload) {
			capacity := min(MaxAccessUnitBytes, max(needed, cap(a.payload)*2))
			payload := make([]byte, len(a.payload), capacity)
			copy(payload, a.payload)
			a.payload = payload
		}
		a.payload = append(a.payload, frame.Payload...)
		if frame.FrameType == avframe.FrameTypeKeyframe || a.frameType == 0 {
			a.frameType = frame.FrameType
		}
	}
	if pkt.Marker {
		if len(a.payload) > 0 {
			output = append(output, avframe.NewAVFrame(avframe.MediaTypeVideo, a.codec, a.frameType, 0, 0, a.payload))
		}
		a.payload, a.frameType = nil, 0
		a.packets, a.codecBytes = 0, 0
		a.open = false
	}
	return output, nil
}

func (a *AccessUnitAssembler) boundCodecCapacity() {
	var pending *[]byte
	switch dp := a.dp.(type) {
	case *VP9Depacketizer:
		pending = &dp.buf
	case *AV1Depacketizer:
		pending = &dp.buf
	}
	// These legacy depacketizers grow with append. The input budget bounds
	// length before decoding; release any excess growth capacity afterwards.
	if pending != nil && cap(*pending) > MaxAccessUnitBytes {
		// Reserve the remaining budget once, so small later continuations do
		// not repeatedly trigger append growth and another full-buffer copy.
		buf := make([]byte, len(*pending), MaxAccessUnitBytes)
		copy(buf, *pending)
		*pending = buf
	}
}

func (a *AccessUnitAssembler) codecPending() bool {
	switch dp := a.dp.(type) {
	case *H264Depacketizer:
		return dp.buf != nil
	case *H265Depacketizer:
		return dp.buf != nil
	case *VP8Depacketizer:
		return dp.buf != nil
	case *VP9Depacketizer:
		return dp.buf != nil
	case *AV1Depacketizer:
		return dp.buf != nil
	default:
		return false
	}
}

func (a *AccessUnitAssembler) isContinuation(payload []byte) bool {
	switch a.codec {
	case avframe.CodecH264:
		return len(payload) >= 2 && payload[0]&0x1f == 28 && payload[1]&0x80 == 0
	case avframe.CodecH265:
		return len(payload) >= 3 && (payload[0]>>1)&0x3f == 49 && payload[2]&0x80 == 0
	case avframe.CodecVP8:
		return len(payload) >= 1 && (payload[0]&0x10 == 0 || payload[0]&0x0f != 0)
	case avframe.CodecVP9:
		return len(payload) >= 1 && payload[0]&0x08 == 0
	case avframe.CodecAV1:
		return len(payload) >= 1 && payload[0]&0x80 != 0
	default:
		return false
	}
}
