package analyzer

import (
	"sort"

	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/tools/testkit/report"
)

const (
	// stallThresholdMultiplier defines how many times the median interval a gap
	// must exceed to be considered a stall.
	stallThresholdMultiplier = 3

	// stallWindowSize is the number of recent intervals to keep for median calculation.
	stallWindowSize = 30
)

// stallDetector detects stalls (freezes) in the media stream by monitoring
// DTS intervals per media type.
type stallDetector struct {
	videoTracker *intervalTracker
	audioTracker *intervalTracker
	stalls       []report.StallEvent
}

func newStallDetector() *stallDetector {
	return &stallDetector{
		videoTracker: newIntervalTracker("video"),
		audioTracker: newIntervalTracker("audio"),
	}
}

func (d *stallDetector) feed(frame *avframe.AVFrame) {
	if frame.FrameType == avframe.FrameTypeSequenceHeader {
		return
	}

	var tracker *intervalTracker
	if frame.MediaType == avframe.MediaTypeVideo {
		tracker = d.videoTracker
	} else if frame.MediaType == avframe.MediaTypeAudio {
		tracker = d.audioTracker
	} else {
		return
	}

	if event, detected := tracker.push(frame.DTS); detected {
		d.stalls = append(d.stalls, event)
	}
}

func (d *stallDetector) events() []report.StallEvent {
	return d.stalls
}

// intervalTracker tracks DTS intervals for one media type and detects gaps.
type intervalTracker struct {
	mediaType string
	prevDTS   int64
	hasPrev   bool
	window    []int64 // sliding window of recent intervals (ms)
}

func newIntervalTracker(mediaType string) *intervalTracker {
	return &intervalTracker{
		mediaType: mediaType,
		window:    make([]int64, 0, stallWindowSize),
	}
}

// push adds a new DTS value and returns a StallEvent if a stall is detected.
func (t *intervalTracker) push(dts int64) (report.StallEvent, bool) {
	if !t.hasPrev {
		t.hasPrev = true
		t.prevDTS = dts
		return report.StallEvent{}, false
	}

	interval := dts - t.prevDTS
	t.prevDTS = dts

	if interval < 0 {
		// Non-monotonic DTS; skip this interval.
		return report.StallEvent{}, false
	}

	median := t.median()
	t.addToWindow(interval)

	// Need enough samples to compute a reliable median.
	if median <= 0 {
		return report.StallEvent{}, false
	}

	threshold := median * stallThresholdMultiplier
	if interval > threshold {
		return report.StallEvent{
			TimestampMs: dts,
			GapMs:       float64(interval),
			MediaType:   t.mediaType,
		}, true
	}

	return report.StallEvent{}, false
}

func (t *intervalTracker) addToWindow(interval int64) {
	if len(t.window) >= stallWindowSize {
		// Drop the oldest entry.
		t.window = t.window[1:]
	}
	t.window = append(t.window, interval)
}

// median returns the median of the current sliding window.
func (t *intervalTracker) median() int64 {
	n := len(t.window)
	if n == 0 {
		return 0
	}

	sorted := make([]int64, n)
	copy(sorted, t.window)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}
