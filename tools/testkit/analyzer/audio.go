package analyzer

import (
	"github.com/im-pingo/liveforge/pkg/avframe"
)

// audioAnalyzer tracks audio stream statistics.
type audioAnalyzer struct {
	frameCount   int64
	totalBytes   int64
	firstDTS     int64
	lastDTS      int64
	hasFirstDTS  bool
	prevDTS      int64
	dtsMonotonic bool
}

func newAudioAnalyzer() *audioAnalyzer {
	return &audioAnalyzer{
		dtsMonotonic: true,
	}
}

func (a *audioAnalyzer) feed(frame *avframe.AVFrame) {
	// Skip sequence headers for frame counting and bitrate.
	if frame.FrameType == avframe.FrameTypeSequenceHeader {
		return
	}

	a.frameCount++
	a.totalBytes += int64(len(frame.Payload))

	if !a.hasFirstDTS {
		a.firstDTS = frame.DTS
		a.lastDTS = frame.DTS
		a.prevDTS = frame.DTS
		a.hasFirstDTS = true
	} else {
		if frame.DTS < a.prevDTS {
			a.dtsMonotonic = false
		}
		a.prevDTS = frame.DTS
		a.lastDTS = frame.DTS
	}
}

// bitrateKbps calculates bitrate in kilobits per second.
func (a *audioAnalyzer) bitrateKbps() float64 {
	durationMs := a.lastDTS - a.firstDTS
	if durationMs <= 0 {
		return 0
	}
	return float64(a.totalBytes*8) / float64(durationMs)
}

// durationMs returns the time span of audio frames.
func (a *audioAnalyzer) durationMs() int64 {
	if !a.hasFirstDTS {
		return 0
	}
	return a.lastDTS - a.firstDTS
}
