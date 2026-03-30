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

func TestSlowConsumerFilterDisabled(t *testing.T) {
	cfg := testSlowConsumerConfig()
	cfg.Enabled = false

	rb := util.NewRingBuffer[*avframe.AVFrame](100)
	writeFrames(rb, 90)
	rb.Close()

	reader := rb.NewReaderAt(0)
	filter := NewSlowConsumerFilter(reader, cfg)
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
	filter := NewSlowConsumerFilter(reader, cfg)

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
	filter := NewSlowConsumerFilter(reader, cfg)
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
	filter := NewSlowConsumerFilter(reader, cfg)
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
	filter := NewSlowConsumerFilter(reader, cfg)
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
	filter := NewSlowConsumerFilter(reader, cfg)

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
		filter := NewSlowConsumerFilter(reader, cfg)
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
		writeFrames(rb, 60) // writer at 60
		reader := rb.NewReaderAt(0) // reader at 0, lag = 0.6
		filter := NewSlowConsumerFilter(reader, cfg)
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
		writeFrames(rb, 40) // writer at 40
		reader := rb.NewReaderAt(0) // reader at 0, lag = 0.4
		filter := NewSlowConsumerFilter(reader, cfg)
		filter.state = ConsumerStateDropNonKey
		buildSlow(filter)

		filter.updateState()
		if filter.State() != ConsumerStateNormal {
			t.Errorf("expected Normal after recovery (lag=0.4), got %s", filter.State())
		}
	})
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
