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
	"github.com/im-pingo/liveforge/pkg/codec/aac"
	"github.com/im-pingo/liveforge/pkg/muxer/fmp4"
)

// DASHSegment holds a single fMP4 media segment (moof+mdat).
type DASHSegment struct {
	SeqNum   int     // sequential segment number (0-based)
	Duration float64 // seconds
	Data     []byte
}

// DASHManager accumulates separate video-only and audio-only fMP4 segments
// from a stream and serves an MPD manifest with two AdaptationSets.
type DASHManager struct {
	mu sync.RWMutex

	// Separate init segments for video and audio tracks.
	videoInitSeg []byte
	audioInitSeg []byte

	// Separate segment stores for video and audio.
	videoSegments []*DASHSegment
	audioSegments []*DASHSegment

	targetDur   float64
	maxSegments int
	nextSeqNum  int
	seqBase     int // sequence number of first segment in window

	startTime  time.Time // for MPD availabilityStartTime
	dtsBase    int64     // DTS of first frame (ms); aligns timeline with segment data
	timeBase   float64   // cumulative duration (seconds) of trimmed segments; used for SegmentTimeline @t
	streamKey  string
	basePath   string // e.g., "/live/stream1"
	hasAudio   bool   // whether audio track is present
	audioCodec string // e.g., "mp4a.40.2" for MPD codecs attribute
	done       chan struct{}

	// Video metadata extracted from sequence header for MPD Representation.
	videoCodecStr string // e.g., "avc1.640028"
	videoWidth    int
	videoHeight   int

	// Audio metadata for MPD Representation.
	audioSampleRate int
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

// InitFromStream computes the fMP4 init segments synchronously so they are
// available immediately after the manager is created (before Run starts).
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

	// Generate separate init segments: video-only and audio-only.
	videoMuxer := fmp4.NewMuxer(videoCodec, 0)
	videoInit := videoMuxer.Init(videoSeqHeader, nil, videoWidth, videoHeight, 0, 0)

	var audioInit []byte
	if audioCodec != 0 {
		audioMuxer := fmp4.NewMuxer(0, audioCodec)
		audioInit = audioMuxer.Init(nil, audioSeqHeader, 0, 0, audioSampleRate, audioChannels)
	}

	d.mu.Lock()
	d.videoInitSeg = videoInit
	d.audioInitSeg = audioInit
	d.hasAudio = audioCodec != 0
	d.audioCodec = dashAudioCodecString(audioCodec, audioSeqHeader)
	d.videoWidth = videoWidth
	d.videoHeight = videoHeight
	d.videoCodecStr = dashVideoCodecString(videoCodec, videoSeqHeader)
	d.audioSampleRate = audioSampleRate
	d.mu.Unlock()
}

// Run starts the segment accumulation loop. It reads frames from the stream's
// RingBuffer, groups them keyframe-to-keyframe, and produces separate video
// and audio fMP4 segments.
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

	// Create separate muxers for video-only and audio-only segments.
	videoMuxer := fmp4.NewMuxer(videoCodec, 0)
	var audioMuxer *fmp4.Muxer
	if audioCodec != 0 {
		audioMuxer = fmp4.NewMuxer(0, audioCodec)
	}

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

	// Init segments may have already been computed by InitFromStream.
	d.mu.RLock()
	hasInit := d.videoInitSeg != nil
	d.mu.RUnlock()
	if !hasInit {
		videoInit := videoMuxer.Init(videoSeqHeader, nil, videoWidth, videoHeight, 0, 0)
		var audioInit []byte
		if audioMuxer != nil {
			audioInit = audioMuxer.Init(nil, audioSeqHeader, 0, 0, audioSampleRate, audioChannels)
		}
		d.mu.Lock()
		if d.videoInitSeg == nil {
			d.videoInitSeg = videoInit
			d.audioInitSeg = audioInit
			d.hasAudio = audioCodec != 0
			d.audioCodec = dashAudioCodecString(audioCodec, audioSeqHeader)
		}
		d.mu.Unlock()
	} else {
		videoMuxer.Init(videoSeqHeader, nil, videoWidth, videoHeight, 0, 0)
		if audioMuxer != nil {
			audioMuxer.Init(nil, audioSeqHeader, 0, 0, audioSampleRate, audioChannels)
		}
	}

	var currentVideoFrames []*avframe.AVFrame
	var currentAudioFrames []*avframe.AVFrame
	var segStartDTS int64
	hasData := false

	// Helper: finalize current segment into separate video and audio segments.
	finalize := func(endDTS int64) {
		if len(currentVideoFrames) == 0 && len(currentAudioFrames) == 0 {
			return
		}
		dur := float64(endDTS-segStartDTS) / 1000.0
		if dur <= 0 {
			dur = d.targetDur
		}

		if d.dtsBase == 0 {
			d.dtsBase = segStartDTS
		}

		// Build video segment with rebased DTS.
		var videoData []byte
		if len(currentVideoFrames) > 0 {
			rebased := make([]*avframe.AVFrame, len(currentVideoFrames))
			for i, f := range currentVideoFrames {
				rf := *f
				rf.DTS -= d.dtsBase
				rf.PTS -= d.dtsBase
				rebased[i] = &rf
			}
			videoData = videoMuxer.WriteSegment(rebased)
		}

		// Build audio segment with rebased DTS.
		var audioData []byte
		if audioMuxer != nil && len(currentAudioFrames) > 0 {
			rebased := make([]*avframe.AVFrame, len(currentAudioFrames))
			for i, f := range currentAudioFrames {
				rf := *f
				rf.DTS -= d.dtsBase
				rf.PTS -= d.dtsBase
				rebased[i] = &rf
			}
			audioData = audioMuxer.WriteSegment(rebased)
		}

		d.mu.Lock()
		if len(videoData) > 0 {
			d.videoSegments = append(d.videoSegments, &DASHSegment{
				SeqNum:   d.nextSeqNum,
				Duration: dur,
				Data:     videoData,
			})
		}
		if len(audioData) > 0 {
			d.audioSegments = append(d.audioSegments, &DASHSegment{
				SeqNum:   d.nextSeqNum,
				Duration: dur,
				Data:     audioData,
			})
		}
		d.nextSeqNum++

		keepCount := d.maxSegments + 5
		if len(d.videoSegments) > keepCount {
			excess := len(d.videoSegments) - keepCount
			for _, seg := range d.videoSegments[:excess] {
				d.timeBase += seg.Duration
			}
			d.videoSegments = d.videoSegments[excess:]
			d.seqBase += excess
		}
		if len(d.audioSegments) > keepCount {
			d.audioSegments = d.audioSegments[len(d.audioSegments)-keepCount:]
		}
		d.mu.Unlock()

		currentVideoFrames = currentVideoFrames[:0]
		currentAudioFrames = currentAudioFrames[:0]
	}

	// Process GOP cache.
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
		if f.MediaType.IsVideo() {
			currentVideoFrames = append(currentVideoFrames, f)
		} else if f.MediaType.IsAudio() {
			currentAudioFrames = append(currentAudioFrames, f)
		}
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
		if gopEndDTS > 0 && frame.DTS <= gopEndDTS {
			continue
		}

		// Split on video keyframes
		if frame.MediaType.IsVideo() && frame.FrameType.IsKeyframe() && hasData && (len(currentVideoFrames) > 0 || len(currentAudioFrames) > 0) {
			finalize(frame.DTS)
			segStartDTS = frame.DTS
		}

		if !hasData {
			segStartDTS = frame.DTS
			hasData = true
		}

		if frame.MediaType.IsVideo() {
			currentVideoFrames = append(currentVideoFrames, frame)
		} else if frame.MediaType.IsAudio() {
			currentAudioFrames = append(currentAudioFrames, frame)
		}
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

// GetInitSegment returns the video init segment (ftyp+moov).
func (d *DASHManager) GetInitSegment() ([]byte, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.videoInitSeg == nil {
		return nil, false
	}
	return d.videoInitSeg, true
}

// GetAudioInitSegment returns the audio init segment (ftyp+moov).
func (d *DASHManager) GetAudioInitSegment() ([]byte, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.audioInitSeg == nil {
		return nil, false
	}
	return d.audioInitSeg, true
}

// GetSegment returns the video media segment by sequence number.
func (d *DASHManager) GetSegment(seqNum int) ([]byte, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	for _, seg := range d.videoSegments {
		if seg.SeqNum == seqNum {
			return seg.Data, true
		}
	}
	return nil, false
}

// GetAudioSegment returns the audio media segment by sequence number.
func (d *DASHManager) GetAudioSegment(seqNum int) ([]byte, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	for _, seg := range d.audioSegments {
		if seg.SeqNum == seqNum {
			return seg.Data, true
		}
	}
	return nil, false
}

// GenerateMPD returns the current DASH MPD manifest with separate video and
// audio AdaptationSets using SegmentTimeline for explicit per-segment timing.
// This avoids the wall-clock-based segment calculation in ffmpeg's dashdec.c
// which, combined with server-side availability holds, caused playback stutters.
func (d *DASHManager) GenerateMPD() string {
	d.mu.RLock()
	defer d.mu.RUnlock()

	segs := d.videoSegments
	audioSegs := d.audioSegments

	var totalDur float64
	for _, seg := range segs {
		totalDur += seg.Duration
	}

	bufferDepth := totalDur
	if bufferDepth < d.targetDur*3 {
		bufferDepth = d.targetDur * 3
	}

	// Compute average segment duration for minimumUpdatePeriod.
	avgDurMs := int(d.targetDur * 1000)
	if len(segs) > 2 {
		var stableDur float64
		for _, seg := range segs[2:] {
			stableDur += seg.Duration
		}
		avgDurMs = int(math.Round(stableDur / float64(len(segs)-2) * 1000))
	} else if len(segs) > 0 {
		avgDurMs = int(math.Round(segs[len(segs)-1].Duration * 1000))
	}
	if avgDurMs <= 0 {
		avgDurMs = int(d.targetDur * 1000)
	}

	// startNumber corresponds to the first segment in the current window.
	startNumber := 1
	if len(segs) > 0 {
		startNumber = segs[0].SeqNum + 1 // SeqNum is 0-based; URL numbers are 1-based
	}

	ast := d.startTime

	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	fmt.Fprintf(&sb, `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="dynamic" minimumUpdatePeriod="PT%dS" availabilityStartTime="%s" publishTime="%s" timeShiftBufferDepth="PT%dS" minBufferTime="PT2S" profiles="urn:mpeg:dash:profile:isoff-live:2011">`,
		int(math.Ceil(float64(avgDurMs)/1000.0)),
		ast.Format(time.RFC3339),
		time.Now().UTC().Format(time.RFC3339),
		int(math.Ceil(bufferDepth)),
	)
	sb.WriteString("\n")
	// UTCTiming: embed the current server time directly so dash.js can
	// synchronize its clock without a separate round-trip.
	fmt.Fprintf(&sb, "  <UTCTiming schemeIdUri=\"urn:mpeg:dash:utc:direct:2014\" value=\"%s\"/>\n",
		time.Now().UTC().Format("2006-01-02T15:04:05.000Z"))
	sb.WriteString(`  <Period id="0" start="PT0S">` + "\n")

	// Video AdaptationSet with SegmentTimeline.
	sb.WriteString(`    <AdaptationSet id="0" contentType="video" mimeType="video/mp4" startWithSAP="1" segmentAlignment="true">` + "\n")
	fmt.Fprintf(&sb, `      <SegmentTemplate timescale="1000" startNumber="%d" initialization="%s/vinit.mp4" media="%s/v$Number$.m4s">`,
		startNumber, d.basePath, d.basePath)
	sb.WriteString("\n")
	sb.WriteString("        <SegmentTimeline>\n")
	timeMs := int64(math.Round(d.timeBase * 1000))
	for i, seg := range segs {
		durMs := int64(math.Round(seg.Duration * 1000))
		if i == 0 {
			fmt.Fprintf(&sb, "          <S t=\"%d\" d=\"%d\"/>\n", timeMs, durMs)
		} else {
			fmt.Fprintf(&sb, "          <S d=\"%d\"/>\n", durMs)
		}
		timeMs += durMs
	}
	sb.WriteString("        </SegmentTimeline>\n")
	sb.WriteString("      </SegmentTemplate>\n")
	videoCodecStr := d.videoCodecStr
	if videoCodecStr == "" {
		videoCodecStr = "avc1.640028"
	}
	vw, vh := d.videoWidth, d.videoHeight
	if vw <= 0 {
		vw = 1920
	}
	if vh <= 0 {
		vh = 1080
	}
	fmt.Fprintf(&sb, "      <Representation id=\"0\" bandwidth=\"2000000\" codecs=\"%s\" width=\"%d\" height=\"%d\"/>\n",
		videoCodecStr, vw, vh)
	sb.WriteString("    </AdaptationSet>\n")

	// Audio AdaptationSet with SegmentTimeline.
	if d.hasAudio && len(audioSegs) > 0 {
		audioCodecs := d.audioCodec
		if audioCodecs == "" {
			audioCodecs = "mp4a.40.2"
		}
		audioStartNumber := 1
		if len(audioSegs) > 0 {
			audioStartNumber = audioSegs[0].SeqNum + 1
		}
		sb.WriteString(`    <AdaptationSet id="1" contentType="audio" mimeType="audio/mp4" startWithSAP="1" segmentAlignment="true">` + "\n")
		fmt.Fprintf(&sb, `      <SegmentTemplate timescale="1000" startNumber="%d" initialization="%s/audio_init.mp4" media="%s/a$Number$.m4s">`,
			audioStartNumber, d.basePath, d.basePath)
		sb.WriteString("\n")
		sb.WriteString("        <SegmentTimeline>\n")
		audioTimeMs := int64(math.Round(d.timeBase * 1000))
		for i, seg := range audioSegs {
			durMs := int64(math.Round(seg.Duration * 1000))
			if i == 0 {
				fmt.Fprintf(&sb, "          <S t=\"%d\" d=\"%d\"/>\n", audioTimeMs, durMs)
			} else {
				fmt.Fprintf(&sb, "          <S d=\"%d\"/>\n", durMs)
			}
			audioTimeMs += durMs
		}
		sb.WriteString("        </SegmentTimeline>\n")
		sb.WriteString("      </SegmentTemplate>\n")
		asr := d.audioSampleRate
		if asr <= 0 {
			asr = 44100
		}
		fmt.Fprintf(&sb, `      <Representation id="1" bandwidth="128000" codecs="%s" audioSamplingRate="%d"/>`, audioCodecs, asr)
		sb.WriteString("\n")
		sb.WriteString("    </AdaptationSet>\n")
	}

	sb.WriteString("  </Period>\n")
	sb.WriteString("</MPD>\n")

	return sb.String()
}

// SegmentCount returns the number of video segments currently available.
func (d *DASHManager) SegmentCount() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.videoSegments)
}

// SegmentRange returns the SeqNum range [lo, hi] of video segments in memory.
func (d *DASHManager) SegmentRange() (lo, hi int) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if len(d.videoSegments) == 0 {
		return -1, -1
	}
	return d.videoSegments[0].SeqNum, d.videoSegments[len(d.videoSegments)-1].SeqNum
}

// dashVideoCodecString returns the DASH codecs string (e.g., "avc1.640028")
// extracted from the video sequence header. Falls back to a sensible default
// if the header is unavailable or the codec is not recognized.
func dashVideoCodecString(codec avframe.CodecType, seqHeader *avframe.AVFrame) string {
	if seqHeader != nil && codec == avframe.CodecH264 && len(seqHeader.Payload) >= 4 {
		// AVCDecoderConfigurationRecord: [1]=profile, [2]=constraint, [3]=level
		return fmt.Sprintf("avc1.%02x%02x%02x",
			seqHeader.Payload[1], seqHeader.Payload[2], seqHeader.Payload[3])
	}
	switch codec {
	case avframe.CodecH264:
		return "avc1.640028" // fallback: High profile, level 4.0
	case avframe.CodecH265:
		return "hvc1.1.6.L120.B0" // fallback: Main profile, level 4.0
	default:
		return "avc1.640028"
	}
}

// dashAudioCodecString returns the DASH codecs string for an audio codec.
// For AAC it parses the AudioSpecificConfig to extract the actual audioObjectType
// so the codec string (e.g. "mp4a.40.2" for AAC-LC, "mp4a.40.5" for HE-AAC)
// matches the ESDS in the init segment. Chrome MSE validates this match.
func dashAudioCodecString(codec avframe.CodecType, audioSeqHeader *avframe.AVFrame) string {
	switch codec {
	case avframe.CodecAAC:
		aot := 2 // default: AAC-LC
		if audioSeqHeader != nil && len(audioSeqHeader.Payload) >= 2 {
			if info, err := aac.ParseAudioSpecificConfig(audioSeqHeader.Payload); err == nil {
				aot = info.ObjectType
			}
		}
		return fmt.Sprintf("mp4a.40.%d", aot)
	case avframe.CodecOpus:
		return "opus"
	case avframe.CodecMP3:
		return "mp4a.40.34"
	default:
		return "mp4a.40.2"
	}
}
