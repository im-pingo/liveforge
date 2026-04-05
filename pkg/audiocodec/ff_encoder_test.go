package audiocodec

import "testing"

func TestFFmpegEncoderPCMU(t *testing.T) {
	enc := NewFFmpegEncoder("pcm_mulaw", 8000, 1)
	defer enc.Close()
	if enc.SampleRate() != 8000 {
		t.Fatalf("expected 8000, got %d", enc.SampleRate())
	}
	if enc.Channels() != 1 {
		t.Fatalf("expected 1, got %d", enc.Channels())
	}
	pcm := &PCMFrame{Samples: make([]int16, 160), SampleRate: 8000, Channels: 1}
	payload, err := enc.Encode(pcm)
	if err != nil {
		t.Fatalf("encode error: %v", err)
	}
	if len(payload) != 160 {
		t.Fatalf("expected 160 bytes, got %d", len(payload))
	}
}

func TestFFmpegEncoderDecodeRoundTrip(t *testing.T) {
	enc := NewFFmpegEncoder("pcm_mulaw", 8000, 1)
	defer enc.Close()
	dec := NewFFmpegDecoder("pcm_mulaw")
	defer dec.Close()
	original := &PCMFrame{Samples: make([]int16, 160), SampleRate: 8000, Channels: 1}
	for i := range original.Samples {
		original.Samples[i] = int16(i * 100)
	}
	encoded, err := enc.Encode(original)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	decoded, err := dec.Decode(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded.Samples) != 160 {
		t.Fatalf("expected 160 samples, got %d", len(decoded.Samples))
	}
	for i := 0; i < 10; i++ {
		diff := int(original.Samples[i]) - int(decoded.Samples[i])
		if diff < -200 || diff > 200 {
			t.Errorf("sample %d: original=%d decoded=%d diff=%d",
				i, original.Samples[i], decoded.Samples[i], diff)
		}
	}
}
