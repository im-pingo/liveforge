package audiocodec

import (
	"testing"

	"github.com/im-pingo/liveforge/pkg/avframe"
)

func TestRoundTripG711(t *testing.T) {
	codecs := []struct {
		name    string
		codec   avframe.CodecType
		encName string
	}{
		{"PCMU", avframe.CodecG711U, "pcm_mulaw"},
		{"PCMA", avframe.CodecG711A, "pcm_alaw"},
	}
	for _, c := range codecs {
		t.Run(c.name, func(t *testing.T) {
			enc := NewFFmpegEncoder(c.encName, 8000, 1)
			defer enc.Close()
			dec := NewFFmpegDecoder(c.encName)
			defer dec.Close()

			pcm := &PCMFrame{
				Samples:    make([]int16, 160),
				SampleRate: 8000,
				Channels:   1,
			}
			for i := range pcm.Samples {
				pcm.Samples[i] = int16(i * 50)
			}

			encoded, err := enc.Encode(pcm)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}

			decoded, err := dec.Decode(encoded)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}

			if len(decoded.Samples) != len(pcm.Samples) {
				t.Fatalf("sample count mismatch: %d vs %d",
					len(decoded.Samples), len(pcm.Samples))
			}
		})
	}
}

func TestRoundTripG722(t *testing.T) {
	enc := NewFFmpegEncoder("g722", 16000, 1)
	defer enc.Close()
	dec := NewFFmpegDecoder("g722")
	defer dec.Close()

	pcm := &PCMFrame{
		Samples:    make([]int16, 320),
		SampleRate: 16000,
		Channels:   1,
	}
	for i := range pcm.Samples {
		pcm.Samples[i] = int16(i * 30)
	}

	encoded, err := enc.Encode(pcm)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	decoded, err := dec.Decode(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(decoded.Samples) == 0 {
		t.Fatal("decoded zero samples")
	}
}

func TestRegistryAllCodecsRegistered(t *testing.T) {
	r := Global()
	codecs := []avframe.CodecType{
		avframe.CodecAAC, avframe.CodecOpus, avframe.CodecMP3,
		avframe.CodecG711U, avframe.CodecG711A, avframe.CodecG722,
		avframe.CodecSpeex,
	}
	for _, c := range codecs {
		if _, err := r.NewDecoder(c); err != nil {
			t.Errorf("no decoder for %s: %v", c, err)
		}
		if _, err := r.NewEncoder(c); err != nil {
			t.Errorf("no encoder for %s: %v", c, err)
		}
	}
}

func TestCanTranscodeMatrix(t *testing.T) {
	r := Global()
	pairs := []struct{ from, to avframe.CodecType }{
		{avframe.CodecAAC, avframe.CodecOpus},
		{avframe.CodecOpus, avframe.CodecAAC},
		{avframe.CodecAAC, avframe.CodecG711U},
		{avframe.CodecG711U, avframe.CodecOpus},
	}
	for _, p := range pairs {
		if !r.CanTranscode(p.from, p.to) {
			t.Errorf("expected CanTranscode(%s, %s) = true", p.from, p.to)
		}
	}
}
