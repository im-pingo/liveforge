package httpstream

import (
	"bytes"
	"log"

	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/codec/aac"
	"github.com/im-pingo/liveforge/pkg/muxer/fmp4"
	"github.com/im-pingo/liveforge/pkg/muxer/ts"
)

// LLHLSSegmenterCallbacks are invoked when segments are produced.
type LLHLSSegmenterCallbacks struct {
	OnInit    func(data []byte)
	OnPart    func(part *LLHLSPart)
	OnSegment func(seg *LLHLSSegment)
}

// LLHLSSegmenter reads AVFrames from a stream and produces LL-HLS segments.
type LLHLSSegmenter struct {
	partDuration float64
	container    string
	callbacks    LLHLSSegmenterCallbacks

	fmp4Muxer  *fmp4.Muxer
	tsMuxer    *ts.Muxer
	partFrames []*avframe.AVFrame // fMP4 frame buffer

	partBuf      bytes.Buffer
	partStartDTS int64
	partHasData  bool
	partIdx      int

	segParts    []*LLHLSPart
	segStartDTS int64
	segMSN      int
	segHasData  bool

	hasVideo         bool // stream contains video track
	gotFirstKeyframe bool // first video keyframe received

	done chan struct{}
}

// NewLLHLSSegmenter creates a new segmenter.
func NewLLHLSSegmenter(partDuration float64, container string, cb LLHLSSegmenterCallbacks) *LLHLSSegmenter {
	return &LLHLSSegmenter{
		partDuration: partDuration,
		container:    container,
		callbacks:    cb,
		done:         make(chan struct{}),
	}
}

// Run starts the segmenter loop. Blocks until stream ends or Stop() is called.
func (s *LLHLSSegmenter) Run(stream *core.Stream) {
	log.Printf("[llhls] segmenter started for %s (container=%s, partDuration=%.3fs)",
		stream.Key(), s.container, s.partDuration)
	defer log.Printf("[llhls] segmenter stopped for %s", stream.Key())

	s.initMuxer(stream)

	// Process GOP cache to pre-populate the first segment so the playlist
	// has content immediately when the first client connects (avoids
	// cold-start stutter).
	startPos := stream.RingBuffer().WriteCursor()
	gopCache := stream.GOPCache()
	var gopEndDTS int64
	for _, f := range gopCache {
		if f.FrameType == avframe.FrameTypeSequenceHeader {
			continue
		}
		gopEndDTS = f.DTS
		s.processFrame(f)
	}
	// Finalize the GOP-based segment so it appears in the playlist right away.
	if s.segHasData {
		s.flushCurrentPart(gopEndDTS)
		s.flushCurrentSegment()
	}

	reader := stream.RingBuffer().NewReaderAt(startPos)
	for {
		select {
		case <-s.done:
			s.flushCurrentPart(s.partStartDTS)
			s.flushCurrentSegment()
			return
		default:
		}

		frame, ok := reader.Read()
		if !ok || frame == nil {
			s.flushCurrentPart(s.partStartDTS)
			s.flushCurrentSegment()
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

		s.processFrame(frame)
	}
}

// Stop signals the segmenter to shut down.
func (s *LLHLSSegmenter) Stop() {
	select {
	case <-s.done:
	default:
		close(s.done)
	}
}

func (s *LLHLSSegmenter) initMuxer(stream *core.Stream) {
	var videoCodec, audioCodec avframe.CodecType
	var videoSeqHeader, audioSeqHeader *avframe.AVFrame

	if vsh := stream.VideoSeqHeader(); vsh != nil {
		videoCodec = vsh.Codec
		videoSeqHeader = vsh
		s.hasVideo = true
	}
	if ash := stream.AudioSeqHeader(); ash != nil {
		audioCodec = ash.Codec
		audioSeqHeader = ash
	}

	switch s.container {
	case "fmp4":
		s.fmp4Muxer = fmp4.NewMuxer(videoCodec, audioCodec)

		var videoWidth, videoHeight int
		if videoSeqHeader != nil && videoCodec == avframe.CodecH264 {
			videoWidth, videoHeight = fmp4.ParseAVCCDimensions(videoSeqHeader.Payload)
		}
		audioSampleRate := 44100
		audioChannels := 2
		if audioSeqHeader != nil {
			if info, err := aac.ParseAudioSpecificConfig(audioSeqHeader.Payload); err == nil {
				audioSampleRate = info.SampleRate
				audioChannels = info.Channels
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
}

func (s *LLHLSSegmenter) processFrame(frame *avframe.AVFrame) {
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

	// Audio-only streams: time-based full segment split (~6s)
	if !isKeyframe && s.segHasData && !frame.MediaType.IsVideo() {
		segElapsed := float64(frame.DTS-s.segStartDTS) / 1000.0
		if segElapsed >= 6.0 {
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
	}
	if !s.segHasData {
		s.segStartDTS = frame.DTS
		s.segHasData = true
	}

	s.muxFrame(frame)
}

func (s *LLHLSSegmenter) muxFrame(frame *avframe.AVFrame) {
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
		return
	}

	dur := float64(endDTS-s.partStartDTS) / 1000.0
	if dur <= 0 {
		dur = s.partDuration
	}

	independent := s.partIdx == 0 && s.segHasData

	part := &LLHLSPart{
		Index:       s.partIdx,
		Duration:    dur,
		Independent: independent,
		Data:        data,
	}
	s.segParts = append(s.segParts, part)
	s.partIdx++
	s.partHasData = false

	if s.callbacks.OnPart != nil {
		s.callbacks.OnPart(part)
	}
}

func (s *LLHLSSegmenter) flushCurrentSegment() {
	if len(s.segParts) == 0 {
		return
	}

	var totalDur float64
	for _, p := range s.segParts {
		totalDur += p.Duration
	}

	seg := &LLHLSSegment{
		MSN:      s.segMSN,
		Duration: totalDur,
		Parts:    s.segParts,
	}

	s.segMSN++
	s.segParts = nil
	s.partIdx = 0
	s.segHasData = false

	if s.callbacks.OnSegment != nil {
		s.callbacks.OnSegment(seg)
	}
}
