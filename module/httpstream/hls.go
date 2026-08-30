package httpstream

import (
	"bytes"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"

	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/muxer/ts"
)

// HLSSegment holds a single TS segment with metadata.
type HLSSegment struct {
	SeqNum        int
	Duration      float64 // seconds
	Data          []byte  // complete TS segment bytes
	Discontinuity bool
}

// HLSManager accumulates TS segments from a stream and serves m3u8 playlists.
type HLSManager struct {
	mu          sync.RWMutex
	segments    []*HLSSegment
	seqBase     int // sequence number of first segment in window
	nextSeqNum  int
	targetDur   float64 // target segment duration in seconds
	maxSegments int     // max segments in sliding window

	streamKey           string
	basePath            string // e.g., "/live/stream1"
	streamInstanceID    uint64
	publisherGeneration uint64
	publisherID         string
	done                chan struct{}
	stopOnce            sync.Once
	inputFactory        segmentInputFactory
	beforeLiveRead      func()
}

// NewHLSManager creates a new HLS manager for a stream.
func NewHLSManager(streamKey, basePath string, targetDur float64, maxSegments int) *HLSManager {
	if targetDur <= 0 {
		targetDur = 6.0
	}
	if maxSegments <= 0 {
		maxSegments = 5
	}
	return &HLSManager{
		streamKey:   streamKey,
		basePath:    basePath,
		targetDur:   targetDur,
		maxSegments: maxSegments,
		done:        make(chan struct{}),
	}
}

// Run starts the segment accumulation loop. It reads frames from the stream's
// RingBuffer, muxes them into TS segments split on video keyframes, and
// maintains a sliding window of recent segments.
func (h *HLSManager) Run(stream *core.Stream) {
	slog.Info("manager started", "module", "hls", "stream", h.streamKey)
	defer slog.Info("manager stopped", "module", "hls", "stream", h.streamKey)

	snapshot, ok := waitStreamStartupForGeneration(h.done, stream, h.streamInstanceID, h.publisherGeneration, h.publisherID)
	if !ok {
		return
	}

	// Use one generation-consistent snapshot for tracks, cached frames, and the
	// live cursor. This keeps a late sequence header from being skipped after
	// the muxer has already been initialized with an empty track.
	gopCache := snapshot.ReplayFrames
	selectedAudioPlan := selectMuxerAudioSnapshot(stream, snapshot, isFlvCompatibleAudio)
	input, audioPlan := newSegmentInputOwner(h.inputFactory, stream, snapshot, selectedAudioPlan, h.done)
	defer input.Close()
	cachedVideoEndDTS, hasCachedVideo := cachedVideoEndDTS(gopCache)

	var videoCodec, audioCodec avframe.CodecType
	var videoSeqData, audioSeqData []byte
	var muxer *ts.Muxer
	var buf bytes.Buffer
	var segStartDTS int64
	hasData := false
	gotFirstKeyframe := false
	mediaClock := segmentMediaClock{}
	recoveryPending := false

	initialize := func(current core.StreamStartupSnapshot, plan muxerAudioPlan) {
		videoCodec, audioCodec = 0, 0
		videoSeqData, audioSeqData = nil, nil
		if vsh := current.VideoSequenceHeader; vsh != nil {
			videoCodec = vsh.Codec
			videoSeqData = vsh.Payload
		}
		if plan.hasAudio() {
			audioCodec = plan.codec
			if plan.sequenceHeader != nil {
				audioSeqData = plan.sequenceHeader.Payload
			}
		}
		audioSampleRate := current.MediaInfo.SampleRate
		if sampleRate, _ := parseAudioSeqHeader(plan.sequenceHeader); sampleRate > 0 {
			audioSampleRate = sampleRate
		}
		muxer = ts.NewMuxer(videoCodec, audioCodec, videoSeqData, audioSeqData)
		gotFirstKeyframe = !videoCodec.IsVideo()
		mediaClock = newSegmentMediaClock(videoCodec.IsVideo(), audioSampleRate)
	}
	initialize(snapshot, audioPlan)

	discardPartial := func() {
		buf.Reset()
		segStartDTS = 0
		hasData = false
		gotFirstKeyframe = !videoCodec.IsVideo()
		mediaClock.Reset()
	}

	// Helper: finalize current segment
	finalize := func(endDTS int64) {
		if buf.Len() == 0 {
			return
		}
		if endDTS <= segStartDTS {
			endDTS = mediaClock.EndDTS(segStartDTS)
		}
		dur := float64(endDTS-segStartDTS) / 1000.0
		seg := &HLSSegment{
			SeqNum:        h.nextSeqNum,
			Duration:      dur,
			Data:          copyBytes(buf.Bytes()),
			Discontinuity: recoveryPending,
		}
		h.mu.Lock()
		h.segments = append(h.segments, seg)
		h.nextSeqNum++
		// Trim sliding window
		if len(h.segments) > h.maxSegments {
			excess := len(h.segments) - h.maxSegments
			h.segments = h.segments[excess:]
			h.seqBase += excess
		}
		h.mu.Unlock()
		buf.Reset()
		mediaClock.Reset()
		recoveryPending = false
	}

	// Process the startup snapshot's GOP into the first segment. The snapshot
	// captured the ring cursor atomically, so the live reader starts after every
	// cached frame.
	// Do not use a cross-track DTS watermark here: audio and video can have
	// different timestamp domains and a late audio frame must not hide a live
	// video frame.
	for _, f := range gopCache {
		if f.FrameType == avframe.FrameTypeSequenceHeader {
			continue
		}
		if !audioPlan.accepts(f) {
			continue
		}
		if !gotFirstKeyframe {
			if !f.MediaType.IsVideo() || !f.FrameType.IsKeyframe() {
				continue
			}
			gotFirstKeyframe = true
		}
		if !hasData {
			segStartDTS = f.DTS
			hasData = true
		}
		if data := muxer.WriteFrame(f); len(data) > 0 {
			buf.Write(data)
		}
		mediaClock.Observe(f)
	}
	// Read live frames.
	for {
		select {
		case <-h.done:
			finalize(segStartDTS) // flush any remaining data
			return
		default:
		}
		if h.beforeLiveRead != nil {
			h.beforeLiveRead()
		}

		read := readSegmentFrame(input.readContext, input.reader, snapshot, input.waitForGenerationOutput)
		if read.Overwritten > 0 {
			action := "continue_audio"
			if videoCodec.IsVideo() {
				action = "wait_keyframe"
			}
			logSegmentOverwrite("hls", action, read.Overwritten)
			discardPartial()
			current, currentOK := currentSegmentSnapshot(stream, snapshot)
			if !currentOK {
				return
			}
			input.reader.AdvanceToLive()
			refreshedAudioPlan := selectMuxerAudioSnapshot(stream, current, isFlvCompatibleAudio)
			if segmentInputSourceChanged(selectedAudioPlan, refreshedAudioPlan) {
				audioPlan = input.Reopen(h.inputFactory, stream, segmentRecoverySnapshot(current), refreshedAudioPlan, h.done)
			} else {
				audioPlan = refreshedAudioPlan
			}
			selectedAudioPlan = refreshedAudioPlan
			initialize(current, audioPlan)
			recoveryPending = true
			continue
		}
		frame := read.Frame
		if !read.OK || frame == nil {
			if managerStopped(h.done) {
				finalize(segStartDTS)
				return
			}
			if stream.IsPublisherGeneration(snapshot.Generation) {
				discardPartial()
				if input.waitForGenerationOutput {
					logSegmentProducerEnd("hls")
				}
				return
			}
			finalize(segStartDTS)
			return
		}
		if !stream.IsPublisherGeneration(snapshot.Generation) {
			if _, ended := snapshot.GenerationEndCursor(); !ended {
				return
			}
		}
		if frame.FrameType == avframe.FrameTypeSequenceHeader {
			continue
		}
		if !audioPlan.accepts(frame) {
			continue
		}
		if isCachedTranscodeVideo(frame, audioPlan, cachedVideoEndDTS, hasCachedVideo) {
			continue
		}
		if !gotFirstKeyframe {
			if !frame.MediaType.IsVideo() || !frame.FrameType.IsKeyframe() {
				continue
			}
			gotFirstKeyframe = true
		}
		if !videoCodec.IsVideo() && hasData && float64(frame.DTS-segStartDTS)/1000.0 >= h.targetDur {
			finalize(frame.DTS)
			segStartDTS = frame.DTS
		}
		// Split on video keyframes (but not the very first frame)
		if frame.MediaType.IsVideo() && frame.FrameType.IsKeyframe() && hasData && buf.Len() > 0 {
			finalize(frame.DTS)
			segStartDTS = frame.DTS
		}

		if !hasData {
			segStartDTS = frame.DTS
			hasData = true
		}

		if !videoCodec.IsVideo() && buf.Len() == 0 {
			buf.Write(muxer.WritePATAndPMT())
		}
		if data := muxer.WriteFrame(frame); len(data) > 0 {
			buf.Write(data)
		}
		mediaClock.Observe(frame)
	}
}

// Stop signals the manager to shut down.
func (h *HLSManager) Stop() {
	h.stopOnce.Do(func() { close(h.done) })
}

// GenerateM3U8 returns the current live m3u8 playlist.
func (h *HLSManager) GenerateM3U8() string {
	h.mu.RLock()
	defer h.mu.RUnlock()

	var sb strings.Builder
	sb.WriteString("#EXTM3U\n")
	sb.WriteString("#EXT-X-VERSION:3\n")

	// Calculate max segment duration for EXT-X-TARGETDURATION
	maxDur := h.targetDur
	for _, seg := range h.segments {
		if seg.Duration > maxDur {
			maxDur = seg.Duration
		}
	}
	fmt.Fprintf(&sb, "#EXT-X-TARGETDURATION:%d\n", int(math.Ceil(maxDur)))

	if len(h.segments) > 0 {
		fmt.Fprintf(&sb, "#EXT-X-MEDIA-SEQUENCE:%d\n", h.segments[0].SeqNum)
	} else {
		sb.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n")
	}

	for _, seg := range h.segments {
		if seg.Discontinuity {
			sb.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		fmt.Fprintf(&sb, "#EXTINF:%.3f,\n", seg.Duration)
		fmt.Fprintf(&sb, "%s/%d.ts\n", h.basePath, seg.SeqNum)
	}

	return sb.String()
}

// GetSegment returns the segment data for a given sequence number.
func (h *HLSManager) GetSegment(seqNum int) ([]byte, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	for _, seg := range h.segments {
		if seg.SeqNum == seqNum {
			return seg.Data, true
		}
	}
	return nil, false
}

// SegmentCount returns the number of segments currently available.
func (h *HLSManager) SegmentCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.segments)
}
