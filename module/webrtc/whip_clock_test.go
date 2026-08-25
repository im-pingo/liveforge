package webrtc

import (
	"testing"
	"time"
)

func TestWHIPMediaClockAlignsIndependentTrackClocks(t *testing.T) {
	clock := newWHIPMediaClock()
	start := time.Unix(100, 0)

	videoStart := clock.DTS(11, 90000, 90000, start)
	audioStart := clock.DTS(22, 48000, 48000, start.Add(20*time.Millisecond))
	if got := audioStart - videoStart; got != 20 {
		t.Fatalf("audio/video start offset = %dms, want 20ms", got)
	}

	videoNext := clock.DTS(11, 90000+90000, 90000, start.Add(time.Second))
	audioNext := clock.DTS(22, 48000+48000, 48000, start.Add(time.Second))
	if got := videoNext - audioNext; got != -20 {
		t.Fatalf("audio/video timeline drift = %dms, want -20ms", got)
	}
}

func TestWHIPMediaClockHandlesRTPWrap(t *testing.T) {
	clock := newWHIPMediaClock()
	start := time.Unix(100, 0)
	base := uint32(^uint32(0) - 89999)
	if got := clock.DTS(11, base, 90000, start); got != 0 {
		t.Fatalf("initial DTS = %d, want 0", got)
	}
	if got := clock.DTS(11, base+90000, 90000, start.Add(time.Second)); got != 1000 {
		t.Fatalf("wrapped DTS = %d, want 1000", got)
	}
}
