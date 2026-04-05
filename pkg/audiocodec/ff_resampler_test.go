package audiocodec

import (
	"math"
	"testing"
)

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

	pcm := &PCMFrame{Samples: make([]int16, 960 * 2), SampleRate: 48000, Channels: 2}
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
