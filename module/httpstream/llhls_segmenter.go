package httpstream

import (
	"bytes"
	"log/slog"
	"sync"

	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/muxer/fmp4"
	"github.com/im-pingo/liveforge/pkg/muxer/ts"
)

// LLHLSSegmenterCallbacks are invoked when segments are produced.
type LLHLSSegmenterCallbacks struct {
	OnInit          func(data []byte)
	OnPart          func(part *LLHLSPart)
	OnSegment       func(seg *LLHLSSegment)
	OnDiscontinuity func()
}

// LLHLSSegmenter reads AVFrames from a stream and produces LL-HLS segments.
type LLHLSSegmenter struct {
	partDuration    float64
	segmentDuration float64
	container       string
	callbacks       LLHLSSegmenterCallbacks

	fmp4Muxer  *fmp4.Muxer
	tsMuxer    *ts.Muxer
	partFrames []*avframe.AVFrame // fMP4 frame buffer

	partBuf         bytes.Buffer
	partStartDTS    int64
	partHasData     bool
	partIndependent bool
	partIdx         int
	partMediaClock  segmentMediaClock

	segParts    []*LLHLSPart
	segStartDTS int64
	segMSN      int
	segHasData  bool

	hasVideo         bool // stream contains video track
	gotFirstKeyframe bool // first video keyframe received
	audioPlan        muxerAudioPlan
	audioPlanSet     bool
	recoveryPending  bool

	done     chan struct{}
	stopOnce sync.Once

	inputFactory   segmentInputFactory
	beforeLiveRead func()
}

// NewLLHLSSegmenter creates a new segmenter.
func NewLLHLSSegmenter(partDuration, segmentDuration float64, container string, cb LLHLSSegmenterCallbacks) *LLHLSSegmenter {
	return &LLHLSSegmenter{
		partDuration:    partDuration,
		segmentDuration: segmentDuration,
		container:       container,
		callbacks:       cb,
		done:            make(chan struct{}),
	}
}

// Run starts the segmenter loop. Blocks until stream ends or Stop() is called.
func (s *LLHLSSegmenter) Run(stream *core.Stream, identity ...any) {
	slog.Info("segmenter started", "module", "llhls", "stream", stream.Key(), "container", s.container, "partDuration", s.partDuration)
	defer slog.Info("segmenter stopped", "module", "llhls", "stream", stream.Key())

	var instanceID, generation uint64
	expectedPublisherID := ""
	if len(identity) > 0 {
		instanceID, _ = identity[0].(uint64)
	}
	if len(identity) > 1 {
		generation, _ = identity[1].(uint64)
	}
	if len(identity) > 2 {
		expectedPublisherID, _ = identity[2].(string)
	}
	snapshot, ok := waitStreamStartupForGeneration(s.done, stream, instanceID, generation, expectedPublisherID)
	if !ok {
		return
	}

	// Process the GOP cache from the same startup snapshot as the muxer tracks
	// and live cursor. This prevents a sequence header arriving just after
	// manager creation from being skipped by the live loop.
	gopCache := snapshot.ReplayFrames
	selectedAudioPlan := s.selectAudioPlanSnapshot(stream, snapshot)
	s.audioPlan = selectedAudioPlan
	s.audioPlanSet = true
	input, audioPlan := newSegmentInputOwner(s.inputFactory, stream, snapshot, s.audioPlan, s.done)
	s.audioPlan = audioPlan
	defer input.Close()
	cachedVideoEndDTS, hasCachedVideo := cachedVideoEndDTS(gopCache)
	s.initMuxerSnapshot(stream, snapshot)

	for _, f := range gopCache {
		if f.FrameType == avframe.FrameTypeSequenceHeader {
			continue
		}
		s.processFrame(f)
	}
	// Publish the cached frames as a part so the initial playlist is playable,
	// but keep the segment open. The atomic snapshot cursor continues in the
	// same GOP, so ending the segment here would either create an interframe-led
	// segment or discard live frames until the next keyframe.
	if s.segHasData {
		s.flushCurrentPart(s.partStartDTS)
	}

	for {
		select {
		case <-s.done:
			s.flushCurrentPart(s.partStartDTS)
			s.flushCurrentSegment()
			return
		default:
		}
		if s.beforeLiveRead != nil {
			s.beforeLiveRead()
		}

		read := readSegmentFrame(input.readContext, input.reader, snapshot, input.waitForGenerationOutput)
		if read.Overwritten > 0 {
			action := "continue_audio"
			if s.hasVideo {
				action = "wait_keyframe"
			}
			logSegmentOverwrite("llhls", action, read.Overwritten)
			s.beginRecovery()
			current, currentOK := currentSegmentSnapshot(stream, snapshot)
			if !currentOK {
				return
			}
			input.reader.AdvanceToLive()
			refreshedAudioPlan := s.selectAudioPlanSnapshot(stream, current)
			if segmentInputSourceChanged(selectedAudioPlan, refreshedAudioPlan) {
				s.audioPlan = input.Reopen(s.inputFactory, stream, segmentRecoverySnapshot(current), refreshedAudioPlan, s.done)
			} else {
				s.audioPlan = refreshedAudioPlan
			}
			selectedAudioPlan = refreshedAudioPlan
			s.audioPlanSet = true
			s.hasVideo = false
			s.initMuxerSnapshot(stream, current)
			s.gotFirstKeyframe = !s.hasVideo
			continue
		}
		frame := read.Frame
		if !read.OK || frame == nil {
			if managerStopped(s.done) {
				s.flushCurrentPart(s.partStartDTS)
				s.flushCurrentSegment()
				return
			}
			if stream.IsPublisherGeneration(snapshot.Generation) {
				s.beginRecovery()
				if input.waitForGenerationOutput {
					logSegmentProducerEnd("llhls")
				}
				return
			}
			s.flushCurrentPart(s.partStartDTS)
			s.flushCurrentSegment()
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
		if isCachedTranscodeVideo(frame, s.audioPlan, cachedVideoEndDTS, hasCachedVideo) {
			continue
		}
		s.processFrame(frame)
	}
}

// Stop signals the segmenter to shut down.
func (s *LLHLSSegmenter) Stop() {
	s.stopOnce.Do(func() { close(s.done) })
}

func (s *LLHLSSegmenter) beginRecovery() {
	s.discardCurrent()
	if !s.recoveryPending {
		s.segMSN++
		if s.callbacks.OnDiscontinuity != nil {
			s.callbacks.OnDiscontinuity()
		}
	}
	s.recoveryPending = true
}

func (s *LLHLSSegmenter) discardCurrent() {
	s.partFrames = nil
	s.partBuf.Reset()
	s.partStartDTS = 0
	s.partHasData = false
	s.partIndependent = false
	s.partIdx = 0
	s.partMediaClock.Reset()
	s.segParts = nil
	s.segStartDTS = 0
	s.segHasData = false
	s.gotFirstKeyframe = !s.hasVideo
}

func (s *LLHLSSegmenter) initMuxer(stream *core.Stream) {
	s.initMuxerSnapshot(stream, stream.StartupSnapshot())
}

func (s *LLHLSSegmenter) initMuxerSnapshot(stream *core.Stream, snapshot core.StreamStartupSnapshot) {
	var videoCodec, audioCodec avframe.CodecType
	var videoSeqHeader, audioSeqHeader *avframe.AVFrame
	if !s.audioPlanSet {
		s.audioPlan = s.selectAudioPlanSnapshot(stream, snapshot)
		s.audioPlanSet = true
	}

	if vsh := snapshot.VideoSequenceHeader; vsh != nil {
		videoCodec = vsh.Codec
		videoSeqHeader = vsh
		s.hasVideo = true
	}
	if s.audioPlan.hasAudio() {
		audioCodec = s.audioPlan.codec
		audioSeqHeader = s.audioPlan.sequenceHeader
	}
	audioSampleRate := snapshot.MediaInfo.SampleRate
	if audioSampleRate <= 0 {
		audioSampleRate = 44100
	}
	if sampleRate, _ := parseAudioSeqHeader(audioSeqHeader); sampleRate > 0 {
		audioSampleRate = sampleRate
	}

	switch s.container {
	case "fmp4":
		s.fmp4Muxer = fmp4.NewMuxer(videoCodec, audioCodec)

		var videoWidth, videoHeight int
		if videoSeqHeader != nil {
			videoWidth, videoHeight = fmp4.ParseVideoDimensions(videoCodec, videoSeqHeader.Payload)
		}
		audioChannels := 2
		if audioSeqHeader != nil {
			if sampleRate, channels := parseAudioSeqHeader(audioSeqHeader); sampleRate > 0 {
				audioChannels = channels
			}
		}

		initSeg := s.fmp4Muxer.Init(videoSeqHeader, audioSeqHeader, videoWidth, videoHeight, audioSampleRate, audioChannels)
		if s.callbacks.OnInit != nil {
			s.callbacks.OnInit(initSeg)
		}

	case "ts":
		var videoSeqData, audioSeqData []byte
		if videoSeqHeader != nil {
			videoSeqData = videoSeqHeader.Payload
		}
		if audioSeqHeader != nil {
			audioSeqData = audioSeqHeader.Payload
		}
		s.tsMuxer = ts.NewMuxer(videoCodec, audioCodec, videoSeqData, audioSeqData)
	}
	s.partMediaClock = newSegmentMediaClock(s.hasVideo, audioSampleRate)
}

func (s *LLHLSSegmenter) selectAudioPlan(stream *core.Stream) muxerAudioPlan {
	return s.selectAudioPlanSnapshot(stream, stream.StartupSnapshot())
}

func (s *LLHLSSegmenter) selectAudioPlanSnapshot(stream *core.Stream, snapshot core.StreamStartupSnapshot) muxerAudioPlan {
	if s.container == "fmp4" {
		return selectFMP4AudioSnapshot(stream, snapshot)
	}
	return selectMuxerAudioSnapshot(stream, snapshot, isFlvCompatibleAudio)
}

func (s *LLHLSSegmenter) processFrame(frame *avframe.AVFrame) {
	if s.audioPlanSet && !s.audioPlan.accepts(frame) {
		return
	}
	isKeyframe := frame.MediaType.IsVideo() && frame.FrameType.IsKeyframe()

	// Wait for first video keyframe before producing any segments.
	// Starting mid-GOP produces undecodable segments (no SPS/PPS for TS,
	// no reference frames for fMP4).
	if !s.gotFirstKeyframe {
		if s.hasVideo {
			if !isKeyframe {
				return
			}
			s.gotFirstKeyframe = true
		} else {
			// Audio-only stream — no keyframe needed
			s.gotFirstKeyframe = true
		}
	}
	if isKeyframe && s.segHasData {
		s.flushCurrentPart(frame.DTS)
		s.flushCurrentSegment()
	}

	// Audio-only streams: time-based full segment split. Audio frames in
	// a video stream must never terminate a keyframe-bounded video segment.
	if !s.hasVideo && s.segHasData && !frame.MediaType.IsVideo() {
		segElapsed := float64(frame.DTS-s.segStartDTS) / 1000.0
		if segElapsed >= s.segmentDuration {
			s.flushCurrentPart(frame.DTS)
			s.flushCurrentSegment()
		}
	}

	// Check if we should flush the current partial based on duration
	if s.partHasData {
		elapsed := float64(frame.DTS-s.partStartDTS) / 1000.0
		if elapsed >= s.partDuration {
			s.flushCurrentPart(frame.DTS)
		}
	}

	if !s.partHasData {
		s.partStartDTS = frame.DTS
		s.partHasData = true
		s.partIndependent = isKeyframe || (!s.hasVideo && s.recoveryPending)
	}
	if !s.segHasData {
		s.segStartDTS = frame.DTS
		s.segHasData = true
	}

	s.muxFrame(frame)
}

func (s *LLHLSSegmenter) muxFrame(frame *avframe.AVFrame) {
	s.partMediaClock.Observe(frame)
	switch s.container {
	case "fmp4":
		s.partFrames = append(s.partFrames, frame)
	case "ts":
		if data := s.tsMuxer.WriteFrame(frame); len(data) > 0 {
			s.partBuf.Write(data)
		}
	}
}

func (s *LLHLSSegmenter) flushCurrentPart(endDTS int64) {
	if !s.partHasData {
		return
	}
	if endDTS <= s.partStartDTS {
		endDTS = s.partMediaClock.EndDTS(s.partStartDTS)
	}

	var data []byte
	switch s.container {
	case "fmp4":
		if len(s.partFrames) > 0 {
			data = s.fmp4Muxer.WriteSegment(s.partFrames)
			s.partFrames = s.partFrames[:0]
		}
	case "ts":
		if s.partBuf.Len() > 0 {
			bufData := s.partBuf.Bytes()
			// The first partial of each segment (partIdx==0) starts with a
			// keyframe. writeVideoFrame already embeds PAT/PMT before
			// keyframes, so we must NOT prepend again — that would create
			// out-of-order continuity counters that break demuxers.
			// Non-keyframe partials (and audio-only streams) need PAT/PMT.
			if s.partIdx > 0 || !s.hasVideo {
				patpmt := s.tsMuxer.WritePATAndPMT()
				data = make([]byte, 0, len(patpmt)+len(bufData))
				data = append(data, patpmt...)
				data = append(data, bufData...)
			} else {
				data = make([]byte, len(bufData))
				copy(data, bufData)
			}
			s.partBuf.Reset()
		}
	}

	if len(data) == 0 {
		s.partHasData = false
		s.partIndependent = false
		s.partMediaClock.Reset()
		return
	}

	dur := float64(endDTS-s.partStartDTS) / 1000.0

	part := &LLHLSPart{
		Index:         s.partIdx,
		Duration:      dur,
		Independent:   s.partIndependent,
		Data:          data,
		Discontinuity: s.recoveryPending,
	}
	s.segParts = append(s.segParts, part)
	s.partIdx++
	s.partHasData = false
	s.partIndependent = false
	s.partMediaClock.Reset()

	if s.callbacks.OnPart != nil {
		s.callbacks.OnPart(part)
	}
	s.recoveryPending = false
}

func (s *LLHLSSegmenter) flushCurrentSegment() {
	if len(s.segParts) == 0 {
		return
	}

	var totalDur float64
	for _, p := range s.segParts {
		totalDur += p.Duration
	}

	discontinuity := false
	for _, part := range s.segParts {
		discontinuity = discontinuity || part.Discontinuity
	}
	seg := &LLHLSSegment{
		MSN:           s.segMSN,
		Duration:      totalDur,
		Parts:         s.segParts,
		Discontinuity: discontinuity,
	}

	s.segMSN++
	s.segParts = nil
	s.partIdx = 0
	s.segHasData = false

	if s.callbacks.OnSegment != nil {
		s.callbacks.OnSegment(seg)
	}
}
