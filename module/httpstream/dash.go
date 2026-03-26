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
	SeqNum   int     // sequential segment number (0-based)
	Duration float64 // seconds
	Data     []byte
}

// DASHManager accumulates fMP4 segments (video + audio) from a stream and
// serves MPD manifests.
type DASHManager struct {
	mu          sync.RWMutex
	initSeg     []byte // ftyp+moov, set once
	segments    []*DASHSegment
	targetDur   float64
	maxSegments int
	nextSeqNum  int
	seqBase     int // sequence number of first segment in window

	startTime  time.Time // for MPD availabilityStartTime
	dtsBase    int64     // DTS of first frame (ms); aligns timeline with segment data
	streamKey  string
	basePath   string // e.g., "/live/stream1"
	hasAudio   bool   // whether audio track is present in init segment
	audioCodec string // e.g., "mp4a.40.2" for MPD codecs attribute
	done       chan struct{}
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

// InitFromStream computes the fMP4 init segment (video + audio) synchronously
// so it is available immediately after the manager is created (before Run starts).
func (d *DASHManager) InitFromStream(stream *core.Stream) {
	var videoCodec, audioCodec avframe.CodecType
	var videoSeqHeader, audioSeqHeader *avframe.AVFrame

	if vsh := stream.VideoSeqHeader(); vsh != nil {
		videoCodec = vsh.Codec
		videoSeqHeader = vsh
	}
	if ash := stream.AudioSeqHeader(); ash != nil {
		audioCodec = ash.Codec
		audioSeqHeader = ash
	}

	muxer := fmp4.NewMuxer(videoCodec, audioCodec)

	var videoWidth, videoHeight int
	if videoSeqHeader != nil && videoCodec == avframe.CodecH264 {
		videoWidth, videoHeight = fmp4.ParseAVCCDimensions(videoSeqHeader.Payload)
	}

	audioSampleRate := 44100
	audioChannels := 2
	if audioSeqHeader != nil {
		if sr, ch := parseAudioSeqHeader(audioSeqHeader); sr > 0 {
			audioSampleRate = sr
			audioChannels = ch
		}
	}

	initSeg := muxer.Init(videoSeqHeader, audioSeqHeader, videoWidth, videoHeight, audioSampleRate, audioChannels)
	d.mu.Lock()
	d.initSeg = initSeg
	d.hasAudio = audioCodec != 0
	d.audioCodec = dashAudioCodecString(audioCodec)
	d.mu.Unlock()
}

// Run starts the segment accumulation loop. It reads frames from the stream's
// RingBuffer, groups them keyframe-to-keyframe, and produces fMP4 segments
// containing both video and audio.
func (d *DASHManager) Run(stream *core.Stream) {
	log.Printf("[dash] manager started for %s", d.streamKey)
	defer log.Printf("[dash] manager stopped for %s", d.streamKey)

	var videoCodec, audioCodec avframe.CodecType
	var videoSeqHeader, audioSeqHeader *avframe.AVFrame

	if vsh := stream.VideoSeqHeader(); vsh != nil {
		videoCodec = vsh.Codec
		videoSeqHeader = vsh
	}
	if ash := stream.AudioSeqHeader(); ash != nil {
		audioCodec = ash.Codec
		audioSeqHeader = ash
	}

	muxer := fmp4.NewMuxer(videoCodec, audioCodec)

	var videoWidth, videoHeight int
	if videoSeqHeader != nil && videoCodec == avframe.CodecH264 {
		videoWidth, videoHeight = fmp4.ParseAVCCDimensions(videoSeqHeader.Payload)
	}

	audioSampleRate := 44100
	audioChannels := 2
	if audioSeqHeader != nil {
		if sr, ch := parseAudioSeqHeader(audioSeqHeader); sr > 0 {
			audioSampleRate = sr
			audioChannels = ch
		}
	}

	// Init segment may have already been computed by InitFromStream.
	d.mu.RLock()
	hasInit := d.initSeg != nil
	d.mu.RUnlock()
	if !hasInit {
		initSeg := muxer.Init(videoSeqHeader, audioSeqHeader, videoWidth, videoHeight, audioSampleRate, audioChannels)
		d.mu.Lock()
		if d.initSeg == nil {
			d.initSeg = initSeg
			d.hasAudio = audioCodec != 0
			d.audioCodec = dashAudioCodecString(audioCodec)
		}
		d.mu.Unlock()
	} else {
		muxer.Init(videoSeqHeader, audioSeqHeader, videoWidth, videoHeight, audioSampleRate, audioChannels)
	}

	var currentFrames []*avframe.AVFrame
	var segStartDTS int64
	hasData := false

	// Helper: finalize current segment
	finalize := func(endDTS int64) {
		if len(currentFrames) == 0 {
			return
		}
		dur := float64(endDTS-segStartDTS) / 1000.0
		if dur <= 0 {
			dur = d.targetDur
		}

		if d.dtsBase == 0 {
			d.dtsBase = segStartDTS
		}

		// Rebase DTS/PTS so baseMediaDecodeTime starts from 0.
		// This avoids the need for presentationTimeOffset in the MPD and
		// aligns segment decode times with the MPD timeline (AST-relative).
		rebasedFrames := make([]*avframe.AVFrame, len(currentFrames))
		for i, f := range currentFrames {
			rf := *f
			rf.DTS -= d.dtsBase
			rf.PTS -= d.dtsBase
			rebasedFrames[i] = &rf
		}

		data := muxer.WriteSegment(rebasedFrames)
		if len(data) == 0 {
			currentFrames = currentFrames[:0]
			return
		}

		seg := &DASHSegment{
			SeqNum:   d.nextSeqNum,
			Duration: dur,
			Data:     data,
		}
		d.mu.Lock()
		d.segments = append(d.segments, seg)
		d.nextSeqNum++
		// Keep extra segments beyond maxSegments as a buffer so that
		// segments referenced in a recently-served MPD are still available
		// when ffplay requests them (race between MPD fetch and segment fetch).
		keepCount := d.maxSegments + 5
		if len(d.segments) > keepCount {
			excess := len(d.segments) - keepCount
			d.segments = d.segments[excess:]
			d.seqBase += excess
		}
		d.mu.Unlock()
		currentFrames = currentFrames[:0]
	}

	// Process GOP cache. Frames are accumulated but NOT finalized as a
	// separate segment — they become part of the first live segment when the
	// next keyframe arrives. This ensures all DASH segments have uniform
	// duration (keyframe-to-keyframe) for SegmentTemplate addressing.
	startPos := stream.RingBuffer().WriteCursor()
	gopCache := stream.GOPCache()
	var gopEndDTS int64
	for _, f := range gopCache {
		if f.FrameType == avframe.FrameTypeSequenceHeader {
			continue
		}
		if !hasData {
			segStartDTS = f.DTS
			hasData = true
		}
		gopEndDTS = f.DTS
		currentFrames = append(currentFrames, f)
	}

	// Read live frames
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
		// Skip frames already included in the GOP cache segment to avoid
		// DTS overlap between the first two segments.
		if gopEndDTS > 0 && frame.DTS <= gopEndDTS {
			continue
		}

		// Split on video keyframes
		if frame.MediaType.IsVideo() && frame.FrameType.IsKeyframe() && hasData && len(currentFrames) > 0 {
			finalize(frame.DTS)
			segStartDTS = frame.DTS
		}

		if !hasData {
			segStartDTS = frame.DTS
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
// with a fixed duration attribute. Segments use rebased DTS (baseMediaDecodeTime
// starts from 0) so no presentationTimeOffset is needed. The player computes
// segment numbers from wall-clock time: num = 1 + floor((now - AST) / duration).
func (d *DASHManager) GenerateMPD() string {
	d.mu.RLock()
	defer d.mu.RUnlock()

	segs := d.segments

	var totalDur float64
	for _, seg := range segs {
		totalDur += seg.Duration
	}

	bufferDepth := totalDur
	if bufferDepth < d.targetDur*3 {
		bufferDepth = d.targetDur * 3
	}

	// Compute the stable segment duration (skip first 2 edge-case segments
	// from GOP cache that may be shorter).
	segDurMs := int(d.targetDur * 1000)
	if len(segs) > 2 {
		var stableDur float64
		for _, seg := range segs[2:] {
			stableDur += seg.Duration
		}
		segDurMs = int(math.Round(stableDur / float64(len(segs)-2) * 1000))
	} else if len(segs) > 0 {
		segDurMs = int(math.Round(segs[len(segs)-1].Duration * 1000))
	}
	if segDurMs <= 0 {
		segDurMs = int(d.targetDur * 1000)
	}

	ast := d.startTime

	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	fmt.Fprintf(&sb, `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="dynamic" minimumUpdatePeriod="PT%dS" availabilityStartTime="%s" publishTime="%s" timeShiftBufferDepth="PT%dS" minBufferTime="PT2S" profiles="urn:mpeg:dash:profile:isoff-live:2011">`,
		int(math.Ceil(float64(segDurMs)/1000.0)),
		ast.Format(time.RFC3339),
		time.Now().UTC().Format(time.RFC3339),
		int(math.Ceil(bufferDepth)),
	)
	sb.WriteString("\n")
	sb.WriteString(`  <Period id="0" start="PT0S">` + "\n")

	sb.WriteString(`    <AdaptationSet id="0" contentType="video" mimeType="video/mp4" startWithSAP="1" segmentAlignment="true">` + "\n")
	// Simple duration addressing: startNumber=1 is constant. Segments use
	// rebased DTS (baseMediaDecodeTime starts from 0), so no PTO is needed.
	// Player computes: segNum = 1 + floor((now - AST) * 1000 / duration).
	fmt.Fprintf(&sb, `      <SegmentTemplate timescale="1000" duration="%d" startNumber="1" initialization="%s/init.mp4" media="%s/$Number$.m4s"/>`,
		segDurMs, d.basePath, d.basePath)
	sb.WriteString("\n")
	sb.WriteString(`      <Representation id="0" bandwidth="2000000" codecs="avc1.640028" width="1920" height="1080"/>` + "\n")
	sb.WriteString("    </AdaptationSet>\n")

	if d.hasAudio {
		audioCodecs := d.audioCodec
		if audioCodecs == "" {
			audioCodecs = "mp4a.40.2"
		}
		sb.WriteString(`    <AdaptationSet id="1" contentType="audio" mimeType="audio/mp4" startWithSAP="1" segmentAlignment="true">` + "\n")
		fmt.Fprintf(&sb, `      <SegmentTemplate timescale="1000" duration="%d" startNumber="1" initialization="%s/init.mp4" media="%s/$Number$.m4s"/>`,
			segDurMs, d.basePath, d.basePath)
		sb.WriteString("\n")
		fmt.Fprintf(&sb, `      <Representation id="1" bandwidth="128000" codecs="%s" audioSamplingRate="44100"/>`, audioCodecs)
		sb.WriteString("\n")
		sb.WriteString("    </AdaptationSet>\n")
	}

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

// SegmentRange returns the SeqNum range [lo, hi] of segments in memory.
func (d *DASHManager) SegmentRange() (lo, hi int) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if len(d.segments) == 0 {
		return -1, -1
	}
	return d.segments[0].SeqNum, d.segments[len(d.segments)-1].SeqNum
}

// SegmentAvailabilityTime returns when the wall clock should have advanced past
// segment seqNum (0-based internal SeqNum). This is the time at which
// ffmpeg's next segment calculation will return seqNum+1 instead of seqNum.
//
// Formula: AST + (seqNum + 1) * segDur, where segDur is the stable segment
// duration in the MPD. If no segments exist yet, returns the zero time.
func (d *DASHManager) SegmentAvailabilityTime(seqNum int) time.Time {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if len(d.segments) == 0 {
		return time.Time{}
	}

	// Compute segment duration the same way GenerateMPD does.
	segDurMs := int(d.targetDur * 1000)
	if len(d.segments) > 2 {
		var stableDur float64
		for _, seg := range d.segments[2:] {
			stableDur += seg.Duration
		}
		segDurMs = int(math.Round(stableDur / float64(len(d.segments)-2) * 1000))
	} else if len(d.segments) > 0 {
		segDurMs = int(math.Round(d.segments[len(d.segments)-1].Duration * 1000))
	}
	if segDurMs <= 0 {
		segDurMs = int(d.targetDur * 1000)
	}

	// URL segment number = seqNum + 1 (1-based startNumber).
	// Next segment calc gives seqNum+1 when now >= AST + (seqNum+1)*segDur.
	urlNum := seqNum + 1
	return d.startTime.Add(time.Duration(urlNum*segDurMs) * time.Millisecond)
}

// dashAudioCodecString returns the DASH codecs string for an audio codec.
func dashAudioCodecString(codec avframe.CodecType) string {
	switch codec {
	case avframe.CodecAAC:
		return "mp4a.40.2"
	case avframe.CodecOpus:
		return "opus"
	case avframe.CodecMP3:
		return "mp4a.40.34"
	default:
		return "mp4a.40.2"
	}
}
