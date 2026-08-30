package core

import (
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/util"
)

func testSlowConsumerConfig() config.SlowConsumerConfig {
	return config.SlowConsumerConfig{
		Enabled:          true,
		LagWarnRatio:     0.5,
		LagDropRatio:     0.75,
		LagCriticalRatio: 0.9,
		LagRecoverRatio:  0.5,
		EWMAAlpha:        0.3,
		SendTimeRatio:    2.0,
	}
}

func makeFrame(mediaType avframe.MediaType, frameType avframe.FrameType) *avframe.AVFrame {
	return avframe.NewAVFrame(mediaType, avframe.CodecH264, frameType, 0, 0, []byte{0x00})
}

// writeFrames writes n video interframes to the ring buffer.
func writeFrames(rb *util.RingBuffer[*avframe.AVFrame], n int) {
	for range n {
		rb.Write(makeFrame(avframe.MediaTypeVideo, avframe.FrameTypeInterframe))
	}
}

// buildSlow feeds slow send reports to trigger isSendSlow().
// Threshold is send_time_ratio * 33ms = 66ms; we report 100ms.
func buildSlow(f *SlowConsumerFilter) {
	for range 10 {
		f.ReportSendTime(100 * time.Millisecond)
	}
}

func TestSlowConsumerFilterReportsFirstOverwriteBeforeDelivery(t *testing.T) {
	// Mutation caught: dropping RingReadResult.Overwritten or deriving it from a
	// later Skipped call hides the first integrity-loss event.
	cfg := testSlowConsumerConfig()
	cfg.Enabled = false
	rb := util.NewRingBuffer[*avframe.AVFrame](2)
	reader := rb.NewReaderAt(0)
	overwrittenFirst := makeFrame(avframe.MediaTypeVideo, avframe.FrameTypeKeyframe)
	overwrittenSecond := makeFrame(avframe.MediaTypeAudio, avframe.FrameTypeInterframe)
	retained := makeFrame(avframe.MediaTypeVideo, avframe.FrameTypeInterframe)
	next := makeFrame(avframe.MediaTypeAudio, avframe.FrameTypeInterframe)
	rb.Write(overwrittenFirst)
	rb.Write(overwrittenSecond)
	rb.Write(retained)
	rb.Write(next)

	filter := NewSlowConsumerFilter(reader, cfg, nil)
	result := filter.NextFrameResult()
	if !result.OK || result.Frame != retained || result.Overwritten != 2 {
		t.Fatalf("first result = %+v, want retained frame %p, OK, overwritten=2", result, retained)
	}

	result = filter.NextFrameResult()
	if !result.OK || result.Frame != next || result.Overwritten != 0 {
		t.Fatalf("continuous result = %+v, want next frame %p, OK, overwritten=0", result, next)
	}
}

func TestSlowConsumerFilterReportsOverwriteBeforePolicy(t *testing.T) {
	// Mutation caught: applying DropNonKey/SkipToKey before returning overwrite
	// metadata can hide the retained interframe from recovery callers.
	tests := []struct {
		name       string
		enabled    bool
		skipCfg    *config.SkipTrackerConfig
		state      ConsumerState
		wantEvents int
	}{
		{name: "nil_tracker", enabled: false, state: ConsumerStateNormal},
		{
			name:    "disabled_tracker",
			enabled: false,
			skipCfg: &config.SkipTrackerConfig{MaxCount: 0, Window: 10 * time.Second},
			state:   ConsumerStateNormal,
		},
		{
			name:       "below_threshold_tracker",
			enabled:    false,
			skipCfg:    &config.SkipTrackerConfig{MaxCount: 2, Window: 10 * time.Second},
			state:      ConsumerStateNormal,
			wantEvents: 1,
		},
		{
			name:    "drop_non_key_would_hide_retained_interframe",
			enabled: true,
			state:   ConsumerStateDropNonKey,
		},
		{
			name:    "skip_to_key_would_hide_retained_interframe",
			enabled: true,
			state:   ConsumerStateSkipToKey,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testSlowConsumerConfig()
			cfg.Enabled = tt.enabled
			rb := util.NewRingBuffer[*avframe.AVFrame](2)
			reader := rb.NewReaderAt(0)
			overwritten := makeFrame(avframe.MediaTypeVideo, avframe.FrameTypeKeyframe)
			retained := makeFrame(avframe.MediaTypeVideo, avframe.FrameTypeInterframe)
			nextKeyframe := makeFrame(avframe.MediaTypeVideo, avframe.FrameTypeKeyframe)
			rb.Write(overwritten)
			rb.Write(retained)
			rb.Write(nextKeyframe)

			filter := NewSlowConsumerFilter(reader, cfg, tt.skipCfg)
			filter.state = tt.state
			result := filter.NextFrameResult()
			if !result.OK || result.Frame != retained || result.Overwritten != 1 {
				t.Fatalf("result = %+v, want retained frame %p, OK, overwritten=1", result, retained)
			}
			if filter.Dropped() != 0 {
				t.Fatalf("overwrite result was filtered before delivery: dropped=%d", filter.Dropped())
			}
			if filter.skipTracker != nil && len(filter.skipTracker.events) != tt.wantEvents {
				t.Fatalf("recorded events = %d, want %d", len(filter.skipTracker.events), tt.wantEvents)
			}
		})
	}
}

func TestSlowConsumerFilterRecordsOverwriteOnceAndPreservesThreshold(t *testing.T) {
	// Mutation caught: recording once in the result helper and again in the
	// compatibility wrapper disconnects one overwrite too early.
	cfg := testSlowConsumerConfig()
	cfg.Enabled = false
	rb := util.NewRingBuffer[*avframe.AVFrame](2)
	reader := rb.NewReaderAt(0)
	filter := NewSlowConsumerFilter(reader, cfg, &config.SkipTrackerConfig{
		MaxCount: 1,
		Window:   10 * time.Second,
	})

	firstRetained := makeFrame(avframe.MediaTypeVideo, avframe.FrameTypeInterframe)
	continuous := makeFrame(avframe.MediaTypeVideo, avframe.FrameTypeKeyframe)
	rb.Write(makeFrame(avframe.MediaTypeVideo, avframe.FrameTypeInterframe))
	rb.Write(firstRetained)
	rb.Write(continuous)

	frame, ok := filter.NextFrame()
	if !ok || frame != firstRetained {
		t.Fatalf("first compatibility read = (%p, %v), want retained frame %p", frame, ok, firstRetained)
	}
	if got := len(filter.skipTracker.events); got != 1 {
		t.Fatalf("events after one compatibility overwrite read = %d, want 1", got)
	}
	continuousResult := filter.NextFrameResult()
	if !continuousResult.OK || continuousResult.Frame != continuous || continuousResult.Overwritten != 0 {
		t.Fatalf("continuous result = %+v, want frame %p, OK, overwritten=0", continuousResult, continuous)
	}
	if got := len(filter.skipTracker.events); got != 1 {
		t.Fatalf("events after one overwrite and one continuous result = %d, want 1", got)
	}

	secondRetained := makeFrame(avframe.MediaTypeVideo, avframe.FrameTypeInterframe)
	rb.Write(makeFrame(avframe.MediaTypeVideo, avframe.FrameTypeInterframe))
	rb.Write(secondRetained)
	rb.Write(makeFrame(avframe.MediaTypeVideo, avframe.FrameTypeKeyframe))
	second := filter.NextFrameResult()
	// Mutation caught: retaining the Ring value after threshold breach lets a
	// frame-first protocol branch send terminal media before checking status.
	wouldSend := second.Frame != nil
	if wouldSend {
		t.Fatalf("threshold result is structurally sendable before status check: %+v", second)
	}
	if second.OK || second.Overwritten != 1 {
		t.Fatalf("threshold result = %+v, want nil frame, not OK, overwritten=1", second)
	}
	if got := len(filter.skipTracker.events); got != 2 {
		t.Fatalf("events at disconnect boundary = %d, want 2", got)
	}
}

func TestSlowConsumerFilterNextFrameCompatibilityAfterResultAPI(t *testing.T) {
	// Mutation caught: returning an overwrite result directly from NextFrame
	// bypasses the established DropNonKey policy.
	cfg := testSlowConsumerConfig()
	rb := util.NewRingBuffer[*avframe.AVFrame](2)
	reader := rb.NewReaderAt(0)
	retainedInterframe := makeFrame(avframe.MediaTypeVideo, avframe.FrameTypeInterframe)
	keyframe := makeFrame(avframe.MediaTypeVideo, avframe.FrameTypeKeyframe)
	rb.Write(makeFrame(avframe.MediaTypeVideo, avframe.FrameTypeInterframe))
	rb.Write(retainedInterframe)
	rb.Write(keyframe)
	rb.Close()

	filter := NewSlowConsumerFilter(reader, cfg, nil)
	filter.state = ConsumerStateDropNonKey
	frame, ok := filter.NextFrame()
	if !ok || frame != keyframe {
		t.Fatalf("NextFrame = (%p, %v), want keyframe %p", frame, ok, keyframe)
	}
	if got := filter.Dropped(); got != 1 {
		t.Fatalf("dropped = %d, want retained interframe counted once", got)
	}
	if frame, ok = filter.NextFrame(); ok || frame != nil {
		t.Fatalf("NextFrame after close = (%p, %v), want (nil, false)", frame, ok)
	}
}

func TestSlowConsumerFilterNextFrameCompatibilitySkipsNil(t *testing.T) {
	// Mutation caught: making NextFrame a direct result-field adapter exposes
	// nil ring values that compatibility callers have always skipped.
	cfg := testSlowConsumerConfig()
	cfg.Enabled = false
	rb := util.NewRingBuffer[*avframe.AVFrame](2)
	reader := rb.NewReaderAt(0)
	want := makeFrame(avframe.MediaTypeAudio, avframe.FrameTypeInterframe)
	rb.Write(nil)
	rb.Write(want)
	rb.Close()

	filter := NewSlowConsumerFilter(reader, cfg, nil)
	frame, ok := filter.NextFrame()
	if !ok || frame != want {
		t.Fatalf("NextFrame = (%p, %v), want non-nil frame %p", frame, ok, want)
	}
}

func TestSlowConsumerFilterNextFrameCompatibilityCloseUnblocks(t *testing.T) {
	// Mutation caught: replacing the blocking Ring result read with polling can
	// lose the reader-close wakeup used by existing compatibility callers.
	cfg := testSlowConsumerConfig()
	rb := util.NewRingBuffer[*avframe.AVFrame](2)
	filter := NewSlowConsumerFilter(rb.NewReaderAt(0), cfg, nil)
	result := make(chan bool, 1)
	go func() {
		_, ok := filter.NextFrame()
		result <- ok
	}()

	filter.Close()
	select {
	case ok := <-result:
		if ok {
			t.Fatal("NextFrame returned OK after filter close")
		}
	case <-time.After(time.Second):
		t.Fatal("NextFrame did not unblock after filter close")
	}
}

func TestSlowConsumerFilterNextFrameResultClosedHasNoEvent(t *testing.T) {
	// Mutation caught: carrying prior overwrite state into an empty/closed read
	// fabricates an integrity event with no available Ring result.
	cfg := testSlowConsumerConfig()
	rb := util.NewRingBuffer[*avframe.AVFrame](2)
	rb.Close()
	filter := NewSlowConsumerFilter(rb.NewReaderAt(0), cfg, nil)

	result := filter.NextFrameResult()
	if result.OK || result.Frame != nil || result.Overwritten != 0 {
		t.Fatalf("closed result = %+v, want zero result", result)
	}
}

func TestSlowConsumerFilterNextFrameResultAllocations(t *testing.T) {
	// Mutation caught: boxing the result, copying the payload, or allocating
	// per-read metadata regresses the immediate normal delivery hot path.
	cfg := testSlowConsumerConfig()
	rb := util.NewRingBuffer[*avframe.AVFrame](2)
	filter := NewSlowConsumerFilter(rb.NewReaderAt(0), cfg, nil)
	frame := makeFrame(avframe.MediaTypeVideo, avframe.FrameTypeKeyframe)
	var result SlowConsumerFrameResult

	allocs := testing.AllocsPerRun(1000, func() {
		rb.Write(frame)
		result = filter.NextFrameResult()
	})
	if allocs != 0 {
		t.Fatalf("NextFrameResult immediate delivery allocations = %f, want 0", allocs)
	}
	t.Logf("NextFrameResult immediate delivery allocations = %f", allocs)
	if !result.OK || result.Frame != frame || result.Overwritten != 0 {
		t.Fatalf("last allocation-run result = %+v, want frame %p, OK, overwritten=0", result, frame)
	}
}

func TestSlowConsumerFilterDisabled(t *testing.T) {
	cfg := testSlowConsumerConfig()
	cfg.Enabled = false

	rb := util.NewRingBuffer[*avframe.AVFrame](100)
	writeFrames(rb, 90)
	rb.Close()

	reader := rb.NewReaderAt(0)
	filter := NewSlowConsumerFilter(reader, cfg, nil)
	buildSlow(filter)

	delivered := 0
	for {
		_, ok := filter.NextFrame()
		if !ok {
			break
		}
		delivered++
	}
	if delivered != 90 {
		t.Errorf("disabled filter should deliver all 90 frames, got %d", delivered)
	}
	if filter.Dropped() != 0 {
		t.Errorf("disabled filter should drop 0 frames, got %d", filter.Dropped())
	}
}

func TestSlowConsumerFilterNormalState(t *testing.T) {
	cfg := testSlowConsumerConfig()
	rb := util.NewRingBuffer[*avframe.AVFrame](100)
	writeFrames(rb, 10)
	rb.Close()

	reader := rb.NewReaderAt(0)
	filter := NewSlowConsumerFilter(reader, cfg, nil)

	delivered := 0
	for {
		_, ok := filter.NextFrame()
		if !ok {
			break
		}
		delivered++
	}
	if delivered != 10 {
		t.Errorf("normal state should deliver all 10 frames, got %d", delivered)
	}
	if filter.State() != ConsumerStateNormal {
		t.Errorf("expected Normal state, got %s", filter.State())
	}
}

func TestSlowConsumerFilterDropNonKey(t *testing.T) {
	cfg := testSlowConsumerConfig()
	rb := util.NewRingBuffer[*avframe.AVFrame](100)

	// Write 80 filler interframes, then specific frames.
	writeFrames(rb, 80)
	rb.Write(makeFrame(avframe.MediaTypeVideo, avframe.FrameTypeKeyframe))
	rb.Write(makeFrame(avframe.MediaTypeVideo, avframe.FrameTypeInterframe))
	rb.Write(makeFrame(avframe.MediaTypeVideo, avframe.FrameTypeInterframe))
	rb.Write(makeFrame(avframe.MediaTypeAudio, avframe.FrameTypeInterframe))
	rb.Write(makeFrame(avframe.MediaTypeVideo, avframe.FrameTypeInterframe))
	rb.Write(makeFrame(avframe.MediaTypeVideo, avframe.FrameTypeSequenceHeader))
	rb.Close()

	reader := rb.NewReaderAt(0)
	filter := NewSlowConsumerFilter(reader, cfg, nil)
	buildSlow(filter)

	keyframes := 0
	audioFrames := 0
	seqHeaders := 0

	for {
		frame, ok := filter.NextFrame()
		if !ok {
			break
		}
		if frame.FrameType.IsKeyframe() && frame.MediaType.IsVideo() {
			keyframes++
		} else if frame.MediaType.IsAudio() {
			audioFrames++
		} else if frame.FrameType == avframe.FrameTypeSequenceHeader {
			seqHeaders++
		}
	}

	if filter.Dropped() == 0 {
		t.Errorf("expected some dropped frames in DropNonKey mode, got 0")
	}
	if keyframes < 1 {
		t.Errorf("expected at least 1 keyframe delivered, got %d", keyframes)
	}
	if audioFrames < 1 {
		t.Errorf("expected at least 1 audio frame delivered, got %d", audioFrames)
	}
	if seqHeaders < 1 {
		t.Errorf("expected at least 1 seq header delivered, got %d", seqHeaders)
	}
}

func TestSlowConsumerFilterSkipToKey(t *testing.T) {
	cfg := testSlowConsumerConfig()
	rb := util.NewRingBuffer[*avframe.AVFrame](100)

	writeFrames(rb, 92)
	rb.Write(makeFrame(avframe.MediaTypeVideo, avframe.FrameTypeKeyframe))
	rb.Close()

	reader := rb.NewReaderAt(0)
	filter := NewSlowConsumerFilter(reader, cfg, nil)
	buildSlow(filter)

	deliveredKeyframe := false
	for {
		frame, ok := filter.NextFrame()
		if !ok {
			break
		}
		if frame.FrameType.IsKeyframe() && frame.MediaType.IsVideo() {
			deliveredKeyframe = true
		}
	}

	if !deliveredKeyframe {
		t.Error("expected keyframe to be delivered after SkipToKey state")
	}
	if filter.Dropped() == 0 {
		t.Error("expected some dropped frames in SkipToKey mode")
	}
}

func TestSlowConsumerFilterRecovery(t *testing.T) {
	cfg := testSlowConsumerConfig()
	rb := util.NewRingBuffer[*avframe.AVFrame](100)

	// Write 80 interframes, then 40 keyframes. Total 120, buffer holds 100.
	// Reader starts at 0, but oldest available is 20 (120-100).
	// Writer at 120, reader clamps to 20, lag = (120-20)/100 = 1.0.
	// As reader consumes keyframes, lag decreases.
	writeFrames(rb, 80)
	for range 40 {
		rb.Write(makeFrame(avframe.MediaTypeVideo, avframe.FrameTypeKeyframe))
	}
	rb.Close()

	reader := rb.NewReaderAt(0)
	filter := NewSlowConsumerFilter(reader, cfg, nil)
	buildSlow(filter)

	// Read all frames until buffer exhausted
	for {
		_, ok := filter.NextFrame()
		if !ok {
			break
		}
	}

	// After exhausting buffer, reader == writer, lag == 0 < 0.5. Should be Normal.
	if filter.State() != ConsumerStateNormal {
		t.Errorf("expected Normal state after recovery, got %s", filter.State())
	}
}

func TestSlowConsumerFilterEWMA(t *testing.T) {
	cfg := testSlowConsumerConfig()
	rb := util.NewRingBuffer[*avframe.AVFrame](100)
	reader := rb.NewReaderAt(0)
	filter := NewSlowConsumerFilter(reader, cfg, nil)

	// First report seeds the EWMA
	filter.ReportSendTime(100 * time.Millisecond)
	if filter.ewmaSend != 100.0 {
		t.Errorf("expected EWMA = 100.0 after first report, got %f", filter.ewmaSend)
	}

	// Second: 0.3 * 50 + 0.7 * 100 = 85
	filter.ReportSendTime(50 * time.Millisecond)
	expected := 0.3*50.0 + 0.7*100.0
	if filter.ewmaSend < expected-0.01 || filter.ewmaSend > expected+0.01 {
		t.Errorf("expected EWMA = %f, got %f", expected, filter.ewmaSend)
	}

	// Third: 0.3 * 200 + 0.7 * 85 = 119.5
	filter.ReportSendTime(200 * time.Millisecond)
	expected = 0.3*200.0 + 0.7*expected
	if filter.ewmaSend < expected-0.01 || filter.ewmaSend > expected+0.01 {
		t.Errorf("expected EWMA = %f, got %f", expected, filter.ewmaSend)
	}
}

func TestSlowConsumerFilterHysteresis(t *testing.T) {
	cfg := testSlowConsumerConfig()

	// Lag at 0.6 (between recover=0.5 and drop=0.75): stays Normal
	t.Run("normal_stays_normal", func(t *testing.T) {
		rb := util.NewRingBuffer[*avframe.AVFrame](100)
		// Write 60 interframes + 1 keyframe so NextFrame has something to deliver
		writeFrames(rb, 59)
		rb.Write(makeFrame(avframe.MediaTypeVideo, avframe.FrameTypeKeyframe))
		rb.Close()
		reader := rb.NewReaderAt(0)
		filter := NewSlowConsumerFilter(reader, cfg, nil)
		buildSlow(filter)

		// Lag = 60/100 = 0.6, below drop threshold (0.75) even with slow EWMA
		filter.NextFrame()
		if filter.State() != ConsumerStateNormal {
			t.Errorf("expected Normal, got %s", filter.State())
		}
	})

	// Test hysteresis: in DropNonKey, lag 0.6 should NOT recover (0.6 > 0.5 recover threshold)
	t.Run("drop_stays_in_dead_zone", func(t *testing.T) {
		rb := util.NewRingBuffer[*avframe.AVFrame](100)
		writeFrames(rb, 60)         // writer at 60
		reader := rb.NewReaderAt(0) // reader at 0, lag = 0.6
		filter := NewSlowConsumerFilter(reader, cfg, nil)
		filter.state = ConsumerStateDropNonKey // force into DropNonKey
		buildSlow(filter)

		// updateState should NOT transition to Normal (lag 0.6 > recover 0.5)
		filter.updateState()
		if filter.State() != ConsumerStateDropNonKey {
			t.Errorf("expected DropNonKey in dead zone (lag=0.6), got %s", filter.State())
		}
	})

	// Test hysteresis: in DropNonKey, lag 0.4 should recover (0.4 < 0.5 recover threshold)
	t.Run("drop_recovers_below_threshold", func(t *testing.T) {
		rb := util.NewRingBuffer[*avframe.AVFrame](100)
		writeFrames(rb, 40)         // writer at 40
		reader := rb.NewReaderAt(0) // reader at 0, lag = 0.4
		filter := NewSlowConsumerFilter(reader, cfg, nil)
		filter.state = ConsumerStateDropNonKey
		buildSlow(filter)

		filter.updateState()
		if filter.State() != ConsumerStateNormal {
			t.Errorf("expected Normal after recovery (lag=0.4), got %s", filter.State())
		}
	})
}

func TestSlowConsumerFilterSkipTrackerDisconnect(t *testing.T) {
	cfg := testSlowConsumerConfig()
	cfg.Enabled = false // disable frame dropping to isolate skip tracker behavior

	// Small ring buffer (size 4). Writing 8 frames with reader at 0 causes overwrite.
	rb := util.NewRingBuffer[*avframe.AVFrame](4)
	reader := rb.NewReaderAt(0)

	skipCfg := &config.SkipTrackerConfig{MaxCount: 2, Window: 10 * time.Second}
	filter := NewSlowConsumerFilter(reader, cfg, skipCfg)

	// Each batch of 8 writes causes one skip event (reader jumps from stale pos to oldest).
	// MaxCount=2 means the 3rd skip should exceed the threshold → disconnect.
	for i := range 3 {
		// Overwrite reader position
		for range 8 {
			rb.Write(makeFrame(avframe.MediaTypeVideo, avframe.FrameTypeInterframe))
		}

		frame, ok := filter.NextFrame()
		if i < 2 {
			if !ok || frame == nil {
				t.Fatalf("skip %d: expected frame delivery, got ok=%v", i+1, ok)
			}
		} else {
			// 3rd skip: should disconnect
			// Mutation caught: adapting NextFrame to return a terminal result's
			// retained frame would violate the legacy (nil, false) contract.
			if ok || frame != nil {
				t.Fatalf("skip %d: expected disconnect (nil, false), got (%p, %v)", i+1, frame, ok)
			}
			return
		}

		// Drain remaining readable frames using TryRead to avoid blocking
		for {
			_, readable := reader.TryRead()
			if !readable {
				break
			}
		}
	}
	t.Fatal("expected disconnect after exceeding skip threshold")
}

func TestSlowConsumerFilterSkipTrackerBelowThreshold(t *testing.T) {
	cfg := testSlowConsumerConfig()
	cfg.Enabled = false

	rb := util.NewRingBuffer[*avframe.AVFrame](4)
	reader := rb.NewReaderAt(0)

	skipCfg := &config.SkipTrackerConfig{MaxCount: 5, Window: 10 * time.Second}
	filter := NewSlowConsumerFilter(reader, cfg, skipCfg)

	// Trigger 2 skip events, well below MaxCount=5
	for range 2 {
		for range 8 {
			rb.Write(makeFrame(avframe.MediaTypeVideo, avframe.FrameTypeInterframe))
		}
		frame, ok := filter.NextFrame()
		if !ok || frame == nil {
			t.Fatal("expected frame delivery when below skip threshold")
		}
		// Drain
		for {
			_, readable := reader.TryRead()
			if !readable {
				break
			}
		}
	}

	// Write one more frame and verify still delivers
	rb.Write(makeFrame(avframe.MediaTypeVideo, avframe.FrameTypeKeyframe))
	rb.Close()
	frame, ok := filter.NextFrame()
	if !ok || frame == nil {
		t.Fatal("expected frame delivery when below skip threshold")
	}
}

func TestSlowConsumerFilterSkipTrackerNil(t *testing.T) {
	cfg := testSlowConsumerConfig()
	cfg.Enabled = false

	rb := util.NewRingBuffer[*avframe.AVFrame](4)
	reader := rb.NewReaderAt(0)

	// nil skipCfg — SkipTracker disabled, should never disconnect due to skips
	filter := NewSlowConsumerFilter(reader, cfg, nil)

	// Trigger many overwrites — should never disconnect
	for range 10 {
		for range 8 {
			rb.Write(makeFrame(avframe.MediaTypeVideo, avframe.FrameTypeInterframe))
		}
		frame, ok := filter.NextFrame()
		if !ok || frame == nil {
			t.Fatal("expected frame delivery with nil SkipTracker (disabled)")
		}
		// Drain
		for {
			_, readable := reader.TryRead()
			if !readable {
				break
			}
		}
	}
}

func TestConsumerStateString(t *testing.T) {
	tests := []struct {
		state ConsumerState
		want  string
	}{
		{ConsumerStateNormal, "normal"},
		{ConsumerStateDropNonKey, "drop_non_key"},
		{ConsumerStateSkipToKey, "skip_to_key"},
		{ConsumerState(99), "unknown"},
	}
	for _, tt := range tests {
		if got := tt.state.String(); got != tt.want {
			t.Errorf("ConsumerState(%d).String() = %q, want %q", tt.state, got, tt.want)
		}
	}
}
