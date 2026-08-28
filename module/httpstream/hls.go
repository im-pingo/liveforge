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
	SeqNum   int
	Duration float64 // seconds
	Data     []byte  // complete TS segment bytes
}

// HLSManager accumulates TS segments from a stream and serves m3u8 playlists.
type HLSManager struct {
	mu          sync.RWMutex
	segments    []*HLSSegment
	seqBase     int // sequence number of first segment in window
	nextSeqNum  int
	targetDur   float64 // target segment duration in seconds
	maxSegments int     // max segments in sliding window

	streamKey   string
	basePath    string // e.g., "/live/stream1"
	publisherID string
	done        chan struct{}
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

	snapshot, ok := waitStreamStartupForPublisher(h.done, stream, h.publisherID)
	if !ok {
		return
	}

	// Use one generation-consistent snapshot for tracks, cached frames, and the
	// live cursor. This keeps a late sequence header from being skipped after
	// the muxer has already been initialized with an empty track.
	gopCache := snapshot.ReplayFrames
	audioPlan := selectMuxerAudioSnapshot(stream, snapshot, isFlvCompatibleAudio)
	reader, release, audioPlan := muxerLiveReaderSnapshot(stream, snapshot, audioPlan)
	defer release()
	cachedVideoEndDTS, hasCachedVideo := cachedVideoEndDTS(gopCache)
	stopReaderWatch := watchRingReader(reader, h.done, snapshot.GenerationDone)
	defer stopReaderWatch()

	var videoCodec, audioCodec avframe.CodecType
	var videoSeqData, audioSeqData []byte

	if vsh := snapshot.VideoSequenceHeader; vsh != nil {
		videoCodec = vsh.Codec
		videoSeqData = vsh.Payload
	}
	if audioPlan.hasAudio() {
		audioCodec = audioPlan.codec
		if audioPlan.sequenceHeader != nil {
			audioSeqData = audioPlan.sequenceHeader.Payload
		}
	}

	muxer := ts.NewMuxer(videoCodec, audioCodec, videoSeqData, audioSeqData)
	var buf bytes.Buffer
	var segStartDTS int64
	hasData := false
	gotFirstKeyframe := !videoCodec.IsVideo()

	// Helper: finalize current segment
	finalize := func(endDTS int64) {
		if buf.Len() == 0 {
			return
		}
		dur := float64(endDTS-segStartDTS) / 1000.0
		if dur <= 0 {
			dur = h.targetDur
		}
		seg := &HLSSegment{
			SeqNum:   h.nextSeqNum,
			Duration: dur,
			Data:     copyBytes(buf.Bytes()),
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
	}

	// Process the startup snapshot's GOP into the first segment. The snapshot
	// captured the ring cursor atomically, so the live reader starts after every
	// cached frame.
	// Do not use a cross-track DTS watermark here: audio and video can have
	// different timestamp domains and a late audio frame must not hide a live
	// video frame.
	for _, f := range gopCache {
		if !stream.IsPublisherGeneration(snapshot.Generation) {
			return
		}
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
	}
	// Read live frames.
	for {
		select {
		case <-h.done:
			finalize(segStartDTS) // flush any remaining data
			return
		default:
		}

		frame, ok := reader.Read()
		if !ok || frame == nil {
			if stream.IsPublisherGeneration(snapshot.Generation) {
				finalize(segStartDTS)
			}
			return
		}
		if !stream.IsPublisherGeneration(snapshot.Generation) {
			return
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
	}
}

// Stop signals the manager to shut down.
func (h *HLSManager) Stop() {
	select {
	case <-h.done:
	default:
		close(h.done)
	}
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
