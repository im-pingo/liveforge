package source

import (
	"bytes"
	_ "embed"
	"io"
	"time"

	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/muxer/flv"
)

// Compile-time interface check.
var _ Source = (*FLVSource)(nil)

//go:embed testdata/source.flv
var embeddedFLV []byte

// FLVSource reads embedded FLV data and emits AVFrames.
// It supports single-pass playback (with optional duration limit) and
// multi-loop playback with monotonically increasing timestamps.
type FLVSource struct {
	duration time.Duration // 0 = no limit (play once through)
	loops    int           // 0 = infinite, >0 = exact count
	looping  bool          // true if this is a loop-mode source

	info *MediaInfo

	// Cached frames from the initial scan.
	frames []*avframe.AVFrame

	// Sequence headers discovered during the scan (re-emitted at loop boundaries).
	videoSeqHeader *avframe.AVFrame
	audioSeqHeader *avframe.AVFrame

	// Source duration: DTS of the last frame plus one frame interval.
	sourceDurationMS int64

	// Playback state.
	cursor     int   // index into the frame-emission sequence
	loopCount  int   // how many loops completed so far
	dtsOffset  int64 // accumulated DTS offset for current loop
	done       bool  // sticky EOF
	emitQueue  []*avframe.AVFrame // pending seq headers to emit at loop boundary
}

// NewFLVSource creates a single-pass FLV source.
// If duration > 0, frames with DTS exceeding the duration are not emitted.
// If duration == 0, all frames are emitted once.
func NewFLVSource(duration time.Duration) *FLVSource {
	s := &FLVSource{
		duration: duration,
		loops:    1,
		looping:  false,
	}
	s.scan()
	return s
}

// NewFLVSourceLoop creates a looping FLV source.
// loops is the number of times to play through. 0 means infinite.
// Sequence headers are re-emitted at each loop boundary, and DTS/PTS
// accumulate monotonically across loops.
func NewFLVSourceLoop(loops int) *FLVSource {
	s := &FLVSource{
		loops:   loops,
		looping: true,
	}
	s.scan()
	return s
}

// scan reads the entire embedded FLV once to discover MediaInfo, cache all
// frames, and capture sequence headers.
func (s *FLVSource) scan() {
	demuxer := flv.NewDemuxer(bytes.NewReader(embeddedFLV))

	var videoCodec, audioCodec avframe.CodecType
	var maxDTS int64

	for {
		frame, err := demuxer.ReadTag()
		if err == io.EOF {
			break
		}
		if err != nil {
			// Embedded test data should always be valid.
			panic("flv_source: scan error: " + err.Error())
		}

		// Discover codecs.
		if frame.MediaType.IsVideo() && videoCodec == 0 {
			videoCodec = frame.Codec
		}
		if frame.MediaType.IsAudio() && audioCodec == 0 {
			audioCodec = frame.Codec
		}

		// Capture sequence headers for loop re-emission.
		if frame.FrameType == avframe.FrameTypeSequenceHeader {
			if frame.MediaType.IsVideo() {
				s.videoSeqHeader = cloneFrame(frame)
			} else {
				s.audioSeqHeader = cloneFrame(frame)
			}
		}

		if frame.DTS > maxDTS {
			maxDTS = frame.DTS
		}

		s.frames = append(s.frames, frame)
	}

	s.info = &MediaInfo{
		VideoCodec: videoCodec,
		AudioCodec: audioCodec,
	}

	// Source duration is the max DTS plus one frame interval, ensuring the
	// next loop starts after the last frame without DTS overlap.
	const loopGapMS = 33 // ~30fps inter-frame gap for DTS continuity at loop boundaries
	s.sourceDurationMS = maxDTS + loopGapMS
}

// NextFrame returns the next AVFrame or io.EOF when the source is exhausted.
func (s *FLVSource) NextFrame() (*avframe.AVFrame, error) {
	if s.done {
		return nil, io.EOF
	}

	// Drain any queued sequence headers first (emitted at loop boundaries).
	if len(s.emitQueue) > 0 {
		f := s.emitQueue[0]
		s.emitQueue = s.emitQueue[1:]
		return f, nil
	}

	// Check if we've reached the end of the current pass.
	if s.cursor >= len(s.frames) {
		if !s.looping {
			s.done = true
			return nil, io.EOF
		}

		s.loopCount++

		// Check loop limit.
		if s.loops > 0 && s.loopCount >= s.loops {
			s.done = true
			return nil, io.EOF
		}

		// Start a new loop: accumulate DTS offset and queue seq headers.
		s.dtsOffset += s.sourceDurationMS
		s.cursor = 0
		s.enqueueSeqHeaders()

		// Re-enter to drain the queue.
		return s.NextFrame()
	}

	orig := s.frames[s.cursor]
	s.cursor++

	// Apply DTS/PTS offset for the current loop.
	f := s.offsetFrame(orig)

	// Duration limit (only for non-looping mode).
	if !s.looping && s.duration > 0 {
		limitMS := s.duration.Milliseconds()
		if f.DTS > limitMS {
			s.done = true
			return nil, io.EOF
		}
	}

	return f, nil
}

// MediaInfo returns the codecs discovered during the initial scan.
func (s *FLVSource) MediaInfo() *MediaInfo {
	return s.info
}

// Reset rewinds the source to the beginning.
func (s *FLVSource) Reset() {
	s.cursor = 0
	s.loopCount = 0
	s.dtsOffset = 0
	s.done = false
	s.emitQueue = nil
}

// enqueueSeqHeaders queues video and audio sequence headers for re-emission
// at a loop boundary. The headers use the current DTS offset so they appear
// at the correct position in the timeline.
func (s *FLVSource) enqueueSeqHeaders() {
	if s.videoSeqHeader != nil {
		s.emitQueue = append(s.emitQueue, s.offsetFrame(s.videoSeqHeader))
	}
	if s.audioSeqHeader != nil {
		s.emitQueue = append(s.emitQueue, s.offsetFrame(s.audioSeqHeader))
	}
}

// offsetFrame creates a new AVFrame with DTS and PTS shifted by the current
// loop's offset. The payload is shared (not copied) since it is read-only.
func (s *FLVSource) offsetFrame(orig *avframe.AVFrame) *avframe.AVFrame {
	return avframe.NewAVFrame(
		orig.MediaType,
		orig.Codec,
		orig.FrameType,
		orig.DTS+s.dtsOffset,
		orig.PTS+s.dtsOffset,
		orig.Payload,
	)
}

// cloneFrame creates a deep copy of an AVFrame (including payload).
func cloneFrame(f *avframe.AVFrame) *avframe.AVFrame {
	payload := make([]byte, len(f.Payload))
	copy(payload, f.Payload)
	return avframe.NewAVFrame(f.MediaType, f.Codec, f.FrameType, f.DTS, f.PTS, payload)
}
