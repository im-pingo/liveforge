package analyzer

import (
	"github.com/im-pingo/liveforge/pkg/avframe"
)

// videoAnalyzer tracks video stream statistics.
type videoAnalyzer struct {
	frameCount   int64
	totalBytes   int64
	firstDTS     int64
	lastDTS      int64
	hasFirstDTS  bool
	prevDTS      int64
	dtsMonotonic bool
	keyframeDTS  []int64
}

func newVideoAnalyzer() *videoAnalyzer {
	return &videoAnalyzer{
		dtsMonotonic: true,
	}
}

func (v *videoAnalyzer) feed(frame *avframe.AVFrame) {
	// Skip sequence headers for frame counting and bitrate.
	if frame.FrameType == avframe.FrameTypeSequenceHeader {
		return
	}

	v.frameCount++
	v.totalBytes += int64(len(frame.Payload))

	if !v.hasFirstDTS {
		v.firstDTS = frame.DTS
		v.lastDTS = frame.DTS
		v.prevDTS = frame.DTS
		v.hasFirstDTS = true
	} else {
		if frame.DTS < v.prevDTS {
			v.dtsMonotonic = false
		}
		v.prevDTS = frame.DTS
		v.lastDTS = frame.DTS
	}

	if frame.FrameType == avframe.FrameTypeKeyframe {
		v.keyframeDTS = append(v.keyframeDTS, frame.DTS)
	}
}

// fps calculates frames per second from DTS range.
func (v *videoAnalyzer) fps() float64 {
	durationMs := v.lastDTS - v.firstDTS
	if durationMs <= 0 || v.frameCount < 2 {
		return 0
	}
	return float64(v.frameCount-1) / (float64(durationMs) / 1000.0)
}

// bitrateKbps calculates bitrate in kilobits per second.
func (v *videoAnalyzer) bitrateKbps() float64 {
	durationMs := v.lastDTS - v.firstDTS
	if durationMs <= 0 {
		return 0
	}
	return float64(v.totalBytes*8) / float64(durationMs)
}

// keyframeInterval returns the average seconds between keyframes.
func (v *videoAnalyzer) keyframeInterval() float64 {
	if len(v.keyframeDTS) < 2 {
		return 0
	}
	totalMs := v.keyframeDTS[len(v.keyframeDTS)-1] - v.keyframeDTS[0]
	return float64(totalMs) / float64(len(v.keyframeDTS)-1) / 1000.0
}

// durationMs returns the time span of video frames.
func (v *videoAnalyzer) durationMs() int64 {
	if !v.hasFirstDTS {
		return 0
	}
	return v.lastDTS - v.firstDTS
}
