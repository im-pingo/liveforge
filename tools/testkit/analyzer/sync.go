package analyzer

import (
	"math"

	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/tools/testkit/report"
)

// syncAnalyzer tracks audio-video synchronization drift.
type syncAnalyzer struct {
	latestVideoDTS int64
	latestAudioDTS int64
	hasVideo       bool
	hasAudio       bool
	maxDrift       float64
	driftSum       float64
	driftCount     int
}

func newSyncAnalyzer() *syncAnalyzer {
	return &syncAnalyzer{}
}

func (s *syncAnalyzer) feed(frame *avframe.AVFrame) {
	if frame.FrameType == avframe.FrameTypeSequenceHeader {
		return
	}

	if frame.MediaType == avframe.MediaTypeVideo {
		s.latestVideoDTS = frame.DTS
		s.hasVideo = true
	} else if frame.MediaType == avframe.MediaTypeAudio {
		s.latestAudioDTS = frame.DTS
		s.hasAudio = true
	}

	// Compute drift only when both streams have been seen.
	if s.hasVideo && s.hasAudio {
		drift := math.Abs(float64(s.latestVideoDTS - s.latestAudioDTS))
		if drift > s.maxDrift {
			s.maxDrift = drift
		}
		s.driftSum += drift
		s.driftCount++
	}
}

func (s *syncAnalyzer) report() report.SyncReport {
	avgDrift := 0.0
	if s.driftCount > 0 {
		avgDrift = s.driftSum / float64(s.driftCount)
	}
	return report.SyncReport{
		MaxDriftMs: s.maxDrift,
		AvgDriftMs: avgDrift,
	}
}
