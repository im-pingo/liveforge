package audiocodec

import (
	"math"
	"testing"
)

func TestFFmpegResampler8kTo48k(t *testing.T) {
	r := NewFFmpegResampler(8000, 1, 48000, 1)
	defer r.Close()
	pcm := &PCMFrame{Samples: make([]int16, 160), SampleRate: 8000, Channels: 1}
	out := r.Resample(pcm)
	if out.SampleRate != 48000 {
		t.Fatalf("expected 48000, got %d", out.SampleRate)
	}
	if math.Abs(float64(len(out.Samples))-960) > 5 {
		t.Fatalf("expected ~960 samples, got %d", len(out.Samples))
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
	expectedSamples := 882 * 2
	if math.Abs(float64(len(out.Samples))-float64(expectedSamples)) > 10 {
		t.Fatalf("expected ~%d samples, got %d", expectedSamples, len(out.Samples))
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
