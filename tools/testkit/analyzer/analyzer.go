// Package analyzer provides protocol-agnostic media stream analysis.
// It receives AVFrame values from any player and produces a PlayReport
// with video/audio statistics, sync measurements, stall detection,
// and codec parameter validation.
package analyzer

import (
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/tools/testkit/report"
)

// Analyzer composes sub-analyzers for comprehensive media stream analysis.
type Analyzer struct {
	video *videoAnalyzer
	audio *audioAnalyzer
	sync  *syncAnalyzer
	stall *stallDetector
	codec *codecValidator
}

// New creates a new Analyzer with all sub-analyzers initialized.
func New() *Analyzer {
	return &Analyzer{
		video: newVideoAnalyzer(),
		audio: newAudioAnalyzer(),
		sync:  newSyncAnalyzer(),
		stall: newStallDetector(),
		codec: newCodecValidator(),
	}
}

// Feed dispatches a frame to the appropriate sub-analyzers based on media type.
func (a *Analyzer) Feed(frame *avframe.AVFrame) {
	// Codec validator receives all frames (looks for sequence headers).
	a.codec.feed(frame)

	// Sync analyzer receives all frames.
	a.sync.feed(frame)

	// Stall detector receives all frames.
	a.stall.feed(frame)

	// Dispatch to media-type-specific analyzers.
	if frame.MediaType == avframe.MediaTypeVideo {
		a.video.feed(frame)
	} else if frame.MediaType == avframe.MediaTypeAudio {
		a.audio.feed(frame)
	}
}

// Report collects results from all sub-analyzers into a PlayReport.
func (a *Analyzer) Report() *report.PlayReport {
	vCodec, vProfile, vLevel, vResolution := a.codec.videoReport()
	aCodec, aSampleRate, aChannels := a.codec.audioReport()

	// Duration is the max of video and audio durations.
	duration := a.video.durationMs()
	if audioDur := a.audio.durationMs(); audioDur > duration {
		duration = audioDur
	}

	return &report.PlayReport{
		Video: report.VideoReport{
			Codec:            vCodec,
			Profile:          vProfile,
			Level:            vLevel,
			Resolution:       vResolution,
			FPS:              a.video.fps(),
			BitrateKbps:      a.video.bitrateKbps(),
			KeyframeInterval: a.video.keyframeInterval(),
			DTSMonotonic:     a.video.dtsMonotonic,
			FrameCount:       a.video.frameCount,
		},
		Audio: report.AudioReport{
			Codec:        aCodec,
			SampleRate:   aSampleRate,
			Channels:     aChannels,
			BitrateKbps:  a.audio.bitrateKbps(),
			DTSMonotonic: a.audio.dtsMonotonic,
			FrameCount:   a.audio.frameCount,
		},
		Sync:       a.sync.report(),
		Stalls:     a.stall.events(),
		Codec:      a.codec.codecReport(),
		DurationMs: duration,
	}
}
