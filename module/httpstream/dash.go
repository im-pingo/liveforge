package httpstream

import (
	"log/slog"
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
	slog.Info("manager started", "module", "dash", "stream", d.streamKey)
	defer slog.Info("manager stopped", "module", "dash", "stream", d.streamKey)

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
	// Returns audio frames that belong to the next segment (DTS >= endDTS).
	finalize := func(endDTS int64) []*avframe.AVFrame {
		if len(currentVideoFrames) == 0 && len(currentAudioFrames) == 0 {
			return nil
		}
		dur := float64(endDTS-segStartDTS) / 1000.0
		if dur <= 0 {
			dur = d.targetDur
		}

		if d.dtsBase == 0 {
			d.dtsBase = segStartDTS
		}

		// Split audio frames at the segment boundary. Audio frames read
		// from the ring buffer before the video keyframe may have DTS
		// values >= endDTS. Including them in this segment would cause
		// DTS overlap with the next segment, triggering ffplay's
		// "DTS out of order" error.
		var segAudio, carryOver []*avframe.AVFrame
		for _, f := range currentAudioFrames {
			if f.DTS >= endDTS {
				carryOver = append(carryOver, f)
			} else {
				segAudio = append(segAudio, f)
			}
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
		if audioMuxer != nil && len(segAudio) > 0 {
			rebased := make([]*avframe.AVFrame, len(segAudio))
			for i, f := range segAudio {
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

		return carryOver
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
			carryOver := finalize(frame.DTS)
			segStartDTS = frame.DTS
			// Carry over audio frames that belong to this new segment.
			if len(carryOver) > 0 {
				currentAudioFrames = append(currentAudioFrames, carryOver...)
			}
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
