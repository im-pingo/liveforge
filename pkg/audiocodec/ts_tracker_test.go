package audiocodec

import "testing"

func TestTsTrackerBasic(t *testing.T) {
	var ts tsTracker
	ts.init(1000, 48000) // base=1000ms, 48kHz

	// First frame: 960 samples (20ms)
	dts := ts.next(960)
	if dts != 1000 {
		t.Fatalf("expected 1000, got %d", dts)
	}

	// Second frame: should be 1000 + 960*1000/48000 = 1020
	dts = ts.next(960)
	if dts != 1020 {
		t.Fatalf("expected 1020, got %d", dts)
	}

	// Third frame: 1000 + 1920*1000/48000 = 1040
	dts = ts.next(960)
	if dts != 1040 {
		t.Fatalf("expected 1040, got %d", dts)
	}
}

func TestTsTracker24HourDrift(t *testing.T) {
	var ts tsTracker
	ts.init(0, 48000)

	// Simulate 24 hours at 20ms per frame = 4,320,000 frames
	frames := 24 * 60 * 60 * 50 // 50 frames/sec
	for i := 0; i < frames; i++ {
		ts.next(960)
	}
	// After 24h: expected = 86,400,000 ms
	finalDts := ts.next(960)
	expected := int64(86_400_000)
	drift := finalDts - expected
	if drift < -1 || drift > 1 {
		t.Fatalf("drift after 24h: %dms (expected ≤1ms)", drift)
	}
}

func TestTsTracker8kHz(t *testing.T) {
	var ts tsTracker
	ts.init(500, 8000) // G.711: 8kHz

	// 160 samples = 20ms at 8kHz
	dts := ts.next(160)
	if dts != 500 {
		t.Fatalf("expected 500, got %d", dts)
	}
	dts = ts.next(160)
	if dts != 520 {
		t.Fatalf("expected 520, got %d", dts)
	}
}
