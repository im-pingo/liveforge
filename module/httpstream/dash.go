package httpstream

import (
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/muxer/fmp4"

)

// DASHSegment holds a single fMP4 media segment (moof+mdat).
type DASHSegment struct {
	SeqNum    int     // sequential segment number
	StartTime int64   // wall-clock offset from AST in ms (for SegmentTimeline t)
	Duration  float64 // seconds
	Data      []byte
}

// DASHManager accumulates video-only fMP4 segments from a stream and serves
// MPD manifests. Audio is excluded from DASH segments for maximum player
// compatibility (ffplay, dash.js, Shaka). Audio is available via HLS or
// continuous FMP4/FLV streams.
type DASHManager struct {
	mu          sync.RWMutex
	initSeg     []byte // ftyp+moov (video track only), set once
	segments    []*DASHSegment
	targetDur   float64
	maxSegments int
	nextSeqNum  int
	seqBase     int // sequence number of first segment in window

	startTime time.Time // for MPD availabilityStartTime
	streamKey string
	basePath  string // e.g., "/live/stream1"
	done      chan struct{}
}

// NewDASHManager creates a new DASH manager for a stream.
func NewDASHManager(streamKey, basePath string, targetDur float64, maxSegments int) *DASHManager {
	if targetDur <= 0 {
		targetDur = 6.0
	}
	if maxSegments <= 0 {
		maxSegments = 30
	}
	return &DASHManager{
		streamKey:   streamKey,
		basePath:    basePath,
		targetDur:   targetDur,
		maxSegments: maxSegments,
		startTime:   time.Now().UTC(),
		done:        make(chan struct{}),
	}
}

// InitFromStream computes the video-only fMP4 init segment synchronously so
// it is available immediately after the manager is created (before Run starts).
func (d *DASHManager) InitFromStream(stream *core.Stream) {
	var videoCodec avframe.CodecType
	var videoSeqHeader *avframe.AVFrame

	if vsh := stream.VideoSeqHeader(); vsh != nil {
		videoCodec = vsh.Codec
		videoSeqHeader = vsh
	}

	// Video-only muxer — no audio codec
	muxer := fmp4.NewMuxer(videoCodec, 0)

	var videoWidth, videoHeight int
	if videoSeqHeader != nil && videoCodec == avframe.CodecH264 {
		videoWidth, videoHeight = fmp4.ParseAVCCDimensions(videoSeqHeader.Payload)
	}

	initSeg := muxer.Init(videoSeqHeader, nil, videoWidth, videoHeight, 0, 0)
	d.mu.Lock()
	d.initSeg = initSeg
	d.mu.Unlock()
}

// Run starts the segment accumulation loop. It reads video frames from the
// stream's RingBuffer, groups them keyframe-to-keyframe, and produces
// video-only fMP4 segments.
func (d *DASHManager) Run(stream *core.Stream) {
	log.Printf("[dash] manager started for %s", d.streamKey)
	defer log.Printf("[dash] manager stopped for %s", d.streamKey)

	var videoCodec avframe.CodecType
	var videoSeqHeader *avframe.AVFrame

	if vsh := stream.VideoSeqHeader(); vsh != nil {
		videoCodec = vsh.Codec
		videoSeqHeader = vsh
	}

	// Video-only muxer
	muxer := fmp4.NewMuxer(videoCodec, 0)

	var videoWidth, videoHeight int
	if videoSeqHeader != nil && videoCodec == avframe.CodecH264 {
		videoWidth, videoHeight = fmp4.ParseAVCCDimensions(videoSeqHeader.Payload)
	}

	// Init segment may have already been computed by InitFromStream.
	d.mu.RLock()
	hasInit := d.initSeg != nil
	d.mu.RUnlock()
	if !hasInit {
		initSeg := muxer.Init(videoSeqHeader, nil, videoWidth, videoHeight, 0, 0)
		d.mu.Lock()
		if d.initSeg == nil {
			d.initSeg = initSeg
		}
		d.mu.Unlock()
	} else {
		muxer.Init(videoSeqHeader, nil, videoWidth, videoHeight, 0, 0)
	}

	var currentFrames []*avframe.AVFrame
	var segStartDTS int64
	segWallStart := time.Now()
	hasData := false

	// Helper: finalize current segment (video frames only)
	finalize := func(endDTS int64) {
		if len(currentFrames) == 0 {
			return
		}
		dur := float64(endDTS-segStartDTS) / 1000.0
		if dur <= 0 {
			dur = d.targetDur
		}
		data := muxer.WriteSegment(currentFrames)
		if len(data) == 0 {
			currentFrames = currentFrames[:0]
			return
		}

		// Wall-clock-aligned StartTime: first segment uses wall-clock offset
		// from AST; subsequent segments chain from previous segment's end.
		var startTimeMs int64
		d.mu.RLock()
		nSegs := len(d.segments)
		if nSegs > 0 {
			prev := d.segments[nSegs-1]
			startTimeMs = prev.StartTime + int64(prev.Duration*1000)
		} else {
			startTimeMs = segWallStart.Sub(d.startTime).Milliseconds()
		}
		d.mu.RUnlock()

		seg := &DASHSegment{
			SeqNum:    d.nextSeqNum,
			StartTime: startTimeMs,
			Duration:  dur,
			Data:      data,
		}
		d.mu.Lock()
		d.segments = append(d.segments, seg)
		d.nextSeqNum++
		if len(d.segments) > d.maxSegments {
			excess := len(d.segments) - d.maxSegments
			d.segments = d.segments[excess:]
			d.seqBase += excess
		}
		d.mu.Unlock()
		currentFrames = currentFrames[:0]
	}

	// Process GOP cache (video frames only)
	startPos := stream.RingBuffer().WriteCursor()
	gopCache := stream.GOPCache()
	for _, f := range gopCache {
		if f.FrameType == avframe.FrameTypeSequenceHeader {
			continue
		}
		if !f.MediaType.IsVideo() {
			continue
		}
		if !hasData {
			segStartDTS = f.DTS
			segWallStart = time.Now()
			hasData = true
		}
		currentFrames = append(currentFrames, f)
	}

	// Read live frames (video only)
	reader := stream.RingBuffer().NewReaderAt(startPos)
	for {
		select {
		case <-d.done:
			finalize(segStartDTS)
			return
		default:
		}

		frame, ok := reader.Read()
		if !ok || frame == nil {
			finalize(segStartDTS)
			return
		}
		if frame.FrameType == avframe.FrameTypeSequenceHeader {
			continue
		}
		// Skip audio frames — DASH segments are video-only
		if !frame.MediaType.IsVideo() {
			continue
		}

		// Split on video keyframes
		if frame.FrameType.IsKeyframe() && hasData && len(currentFrames) > 0 {
			finalize(frame.DTS)
			segStartDTS = frame.DTS
			segWallStart = time.Now()
		}

		if !hasData {
			segStartDTS = frame.DTS
			segWallStart = time.Now()
			hasData = true
		}

		currentFrames = append(currentFrames, frame)
	}
}

// Stop signals the manager to shut down.
func (d *DASHManager) Stop() {
	select {
	case <-d.done:
	default:
		close(d.done)
	}
}

// GetInitSegment returns the fMP4 init segment (ftyp+moov).
func (d *DASHManager) GetInitSegment() ([]byte, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.initSeg == nil {
		return nil, false
	}
	return d.initSeg, true
}

// GetSegment returns the media segment by sequence number.
func (d *DASHManager) GetSegment(seqNum int) ([]byte, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	for _, seg := range d.segments {
		if seg.SeqNum == seqNum {
			return seg.Data, true
		}
	}
	return nil, false
}

// GenerateMPD returns the current DASH MPD manifest using $Number$ addressing
// with a SegmentTimeline whose t values are wall-clock-aligned to AST.
func (d *DASHManager) GenerateMPD() string {
	d.mu.RLock()
	defer d.mu.RUnlock()

	// Compute timeShiftBufferDepth from available segments
	var totalDur float64
	for _, seg := range d.segments {
		totalDur += seg.Duration
	}

	// Buffer depth: actual content duration, minimum 3 * targetDur
	bufferDepth := totalDur
	if bufferDepth < d.targetDur*3 {
		bufferDepth = d.targetDur * 3
	}

	startNumber := 1
	if len(d.segments) > 0 {
		startNumber = d.segments[0].SeqNum + 1 // 1-based for ffplay compat
	}

	// suggestedPresentationDelay: 3 segments behind live edge
	presentDelay := int(math.Ceil(d.targetDur * 3))
	if presentDelay < 2 {
		presentDelay = 2
	}

	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	fmt.Fprintf(&sb, `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="dynamic" minimumUpdatePeriod="PT%dS" availabilityStartTime="%s" publishTime="%s" timeShiftBufferDepth="PT%dS" suggestedPresentationDelay="PT%dS" minBufferTime="PT2S" profiles="urn:mpeg:dash:profile:isoff-live:2011">`,
		int(math.Ceil(d.targetDur)),
		d.startTime.Format(time.RFC3339),
		time.Now().UTC().Format(time.RFC3339),
		int(math.Ceil(bufferDepth)),
		presentDelay,
	)
	sb.WriteString("\n")
	sb.WriteString(`  <Period id="0" start="PT0S">` + "\n")

	sb.WriteString(`    <AdaptationSet id="0" contentType="video" mimeType="video/mp4" startWithSAP="1" segmentAlignment="true">` + "\n")
	fmt.Fprintf(&sb, `      <SegmentTemplate timescale="1000" initialization="%s/init.mp4" media="%s/$Number$.m4s" startNumber="%d">`,
		d.basePath, d.basePath, startNumber)
	sb.WriteString("\n")
	sb.WriteString("        <SegmentTimeline>\n")
	for i, seg := range d.segments {
		if i == 0 {
			fmt.Fprintf(&sb, `          <S t="%d" d="%d"/>`, seg.StartTime, int(seg.Duration*1000))
		} else {
			fmt.Fprintf(&sb, `          <S d="%d"/>`, int(seg.Duration*1000))
		}
		sb.WriteString("\n")
	}
	sb.WriteString("        </SegmentTimeline>\n")
	sb.WriteString("      </SegmentTemplate>\n")
	sb.WriteString(`      <Representation id="0" bandwidth="2000000" codecs="avc1.640028" width="1920" height="1080"/>` + "\n")
	sb.WriteString("    </AdaptationSet>\n")
	sb.WriteString("  </Period>\n")
	sb.WriteString("</MPD>\n")

	return sb.String()
}

// SegmentCount returns the number of segments currently available.
func (d *DASHManager) SegmentCount() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.segments)
}
