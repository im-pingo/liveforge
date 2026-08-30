//go:build audiocodec

package audiocodec

import (
	"math"
	"slices"
	"testing"
)

var _ DrainingResampler = (*FFmpegResampler)(nil)
var _ AttributedDrainingResampler = (*FFmpegResampler)(nil)

func TestFFmpegResamplerDrainReturnsTerminalSamplesExactlyOnce(t *testing.T) {
	input := make([]int16, 160)
	for i := range input {
		input[i] = int16((i*197)%20000 - 10000)
	}
	pcm := &PCMFrame{Samples: input, SampleRate: 8000, Channels: 1}

	r := NewFFmpegResampler(8000, 1, 48000, 1)
	defer r.Close()
	beforeDrain := r.Resample(pcm)
	if len(beforeDrain.Samples) >= len(input)*6 {
		t.Fatalf("streaming resample returned %d samples before drain, want fewer than terminal count %d", len(beforeDrain.Samples), len(input)*6)
	}

	drainer, ok := any(r).(interface{ Drain() *PCMFrame })
	if !ok {
		t.Fatal("FFmpeg resampler does not expose terminal drain")
	}
	tail := drainer.Drain()
	wantTailSamples := len(input)*6 - len(beforeDrain.Samples)
	if len(tail.Samples) != wantTailSamples {
		t.Fatalf("resampler tail samples = %d, want %d", len(tail.Samples), wantTailSamples)
	}

	nonZero := false
	for _, sample := range tail.Samples {
		if sample != 0 {
			nonZero = true
			break
		}
	}
	if !nonZero {
		t.Fatal("terminal resampler tail was replaced entirely by silence")
	}

	again := drainer.Drain()
	if len(again.Samples) != 0 {
		t.Fatalf("second resampler drain returned %d duplicate samples, want 0", len(again.Samples))
	}
}

func TestFFmpegResampler8kTo48k(t *testing.T) {
	r := NewFFmpegResampler(8000, 1, 48000, 1)
	defer r.Close()

	// Single-call output may be smaller than the theoretical ratio due to
	// the resampler's internal filter delay (no flush between calls in
	// streaming mode). Verify the first call produces a reasonable amount.
	pcm := &PCMFrame{Samples: make([]int16, 160), SampleRate: 8000, Channels: 1}
	out := r.Resample(pcm)
	if out.SampleRate != 48000 {
		t.Fatalf("expected 48000, got %d", out.SampleRate)
	}
	// Allow up to 15% fewer samples on the first call (filter warmup).
	if len(out.Samples) < 800 || len(out.Samples) > 1000 {
		t.Fatalf("first call: expected 800-1000 samples, got %d", len(out.Samples))
	}

	// Over multiple calls the cumulative output must converge to the
	// theoretical ratio (48000/8000 = 6x).
	totalIn := 160
	totalOut := len(out.Samples)
	for i := 0; i < 49; i++ {
		out = r.Resample(pcm)
		totalIn += 160
		totalOut += len(out.Samples)
	}
	expected := float64(totalIn) * 48000.0 / 8000.0
	if math.Abs(float64(totalOut)-expected) > expected*0.01 {
		t.Fatalf("cumulative: expected ~%.0f samples, got %d (%.2f%% error)",
			expected, totalOut, math.Abs(float64(totalOut)-expected)/expected*100)
	}
}

func TestFFmpegResampler48kTo44k(t *testing.T) {
	r := NewFFmpegResampler(48000, 2, 44100, 2)
	defer r.Close()

	pcm := &PCMFrame{Samples: make([]int16, 960*2), SampleRate: 48000, Channels: 2}
	out := r.Resample(pcm)
	if out.SampleRate != 44100 {
		t.Fatalf("expected 44100, got %d", out.SampleRate)
	}
	expectedFirst := 882 * 2
	// Allow up to 5% fewer on first call.
	if math.Abs(float64(len(out.Samples))-float64(expectedFirst)) > float64(expectedFirst)*0.05 {
		t.Fatalf("first call: expected ~%d samples, got %d", expectedFirst, len(out.Samples))
	}

	// Verify cumulative accuracy over 50 calls.
	totalIn := 960 // per channel
	totalOut := len(out.Samples) / 2
	for i := 0; i < 49; i++ {
		out = r.Resample(pcm)
		totalIn += 960
		totalOut += len(out.Samples) / 2
	}
	expected := float64(totalIn) * 44100.0 / 48000.0
	if math.Abs(float64(totalOut)-expected) > expected*0.01 {
		t.Fatalf("cumulative: expected ~%.0f per-ch samples, got %d (%.2f%% error)",
			expected, totalOut, math.Abs(float64(totalOut)-expected)/expected*100)
	}
}

func TestFFmpegResamplerMonoToStereo(t *testing.T) {
	r := NewFFmpegResampler(8000, 1, 8000, 2)
	defer r.Close()
	pcm := &PCMFrame{Samples: make([]int16, 160), SampleRate: 8000, Channels: 1}
	out := r.Resample(pcm)
	if out.Channels != 2 {
		t.Fatalf("expected 2 channels, got %d", out.Channels)
	}
	if len(out.Samples) != 320 {
		t.Fatalf("expected 320 samples, got %d", len(out.Samples))
	}
}

// Mutation caught: dropping a zero-output input span, using only the newest
// span for later output, never aging the first span, or re-emitting a drain.
func TestFFmpegResamplerAttributedStreamingAgesMeasuredContributors(t *testing.T) {
	r := NewFFmpegResampler(8000, 1, 48000, 1)
	defer r.Close()

	sawZeroOutput := false
	sawUnionAfterZero := false
	sawFirstSpanAgeOut := false
	for i := 0; i < 80; i++ {
		span := SourceSpan{Begin: int64(i), End: int64(i + 1)}
		out, err := r.ResampleAttributed(&PCMFrame{
			Samples:    []int16{int16(i*257 - 10000)},
			SampleRate: 8000,
			Channels:   1,
		}, span)
		if err != nil {
			t.Fatalf("attributed resample input %d: %v", i, err)
		}
		if len(out.Samples) == 0 {
			sawZeroOutput = true
			if out.SourceSpan.Valid() {
				t.Fatalf("zero-output input %d span = %+v, want invalid result metadata", i, out.SourceSpan)
			}
			continue
		}
		if !out.SourceSpan.Valid() {
			t.Fatalf("non-empty output %d has invalid source span %+v", i, out.SourceSpan)
		}
		if out.SourceSpan.Begin > span.Begin || out.SourceSpan.End < span.End {
			t.Fatalf("output %d span %+v does not cover current input %+v", i, out.SourceSpan, span)
		}
		if sawZeroOutput && out.SourceSpan.Begin == 0 && out.SourceSpan.End == span.End {
			sawUnionAfterZero = true
		}
		if out.SourceSpan.Begin > 0 {
			sawFirstSpanAgeOut = true
		}
	}
	if !sawZeroOutput {
		t.Fatal("fixture produced no zero-output streaming call")
	}
	if !sawUnionAfterZero {
		t.Fatal("first output did not conservatively union retained zero-output and current contributors")
	}
	if !sawFirstSpanAgeOut {
		t.Fatal("first source span never aged out under measured streaming delay")
	}

	tail, err := r.DrainAttributed()
	if err != nil {
		t.Fatalf("attributed resampler drain: %v", err)
	}
	if len(tail.Samples) == 0 {
		t.Fatal("attributed resampler drain returned no terminal samples")
	}
	if !tail.SourceSpan.Valid() || tail.SourceSpan.Begin == 0 || tail.SourceSpan.End != 80 {
		t.Fatalf("terminal source span = %+v, want valid remaining tail ending at 80 with first span aged out", tail.SourceSpan)
	}
	again, err := r.DrainAttributed()
	if err != nil {
		t.Fatalf("second attributed resampler drain: %v", err)
	}
	if len(again.Samples) != 0 || again.SourceSpan.Valid() {
		t.Fatalf("second attributed drain samples/span = %d/%+v, want empty/invalid", len(again.Samples), again.SourceSpan)
	}
}

// Mutation caught: attribution changing streaming or terminal sample content,
// ordering, ownership, or legacy drain behavior.
func TestFFmpegResamplerAttributedMatchesLegacySamples(t *testing.T) {
	legacy := NewFFmpegResampler(8000, 1, 48000, 2)
	defer legacy.Close()
	attributed := NewFFmpegResampler(8000, 1, 48000, 2)
	defer attributed.Close()

	for frame := 0; frame < 4; frame++ {
		pcm := &PCMFrame{
			Samples:    make([]int16, 160),
			SampleRate: 8000,
			Channels:   1,
		}
		for i := range pcm.Samples {
			pcm.Samples[i] = int16(((i+frame*17)%211 - 105) * 127)
		}
		legacyOut := legacy.Resample(pcm)
		attributedOut, err := attributed.ResampleAttributed(
			pcm,
			SourceSpan{Begin: int64(frame + 1), End: int64(frame + 2)},
		)
		if err != nil {
			t.Fatalf("attributed resample frame %d: %v", frame, err)
		}
		if !slices.Equal(attributedOut.Samples, legacyOut.Samples) {
			t.Fatalf("attributed resample frame %d samples differ from legacy", frame)
		}
		if len(attributedOut.Samples) > 0 && !attributedOut.SourceSpan.Valid() {
			t.Fatalf("attributed resample frame %d has invalid span %+v", frame, attributedOut.SourceSpan)
		}
	}

	legacyTail := legacy.Drain()
	attributedTail, err := attributed.DrainAttributed()
	if err != nil {
		t.Fatalf("attributed drain: %v", err)
	}
	if !slices.Equal(attributedTail.Samples, legacyTail.Samples) {
		t.Fatal("attributed resampler terminal samples differ from legacy")
	}
	if len(attributedTail.Samples) > 0 && !attributedTail.SourceSpan.Valid() {
		t.Fatalf("attributed terminal samples have invalid span %+v", attributedTail.SourceSpan)
	}
}
