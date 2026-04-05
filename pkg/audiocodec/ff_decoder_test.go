package audiocodec

import "testing"

func TestFFmpegDecoderPCMU(t *testing.T) {
	dec := NewFFmpegDecoder("pcm_mulaw")
	defer dec.Close()
	if dec.SampleRate() != 8000 {
		t.Fatalf("expected 8000, got %d", dec.SampleRate())
	}
	payload := make([]byte, 160)
	for i := range payload {
		payload[i] = 0xFF
	}
	pcm, err := dec.Decode(payload)
	if err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if len(pcm.Samples) != 160 {
		t.Fatalf("expected 160 samples, got %d", len(pcm.Samples))
	}
	if pcm.SampleRate != 8000 {
		t.Fatalf("expected sample rate 8000, got %d", pcm.SampleRate)
	}
	if pcm.Channels != 1 {
		t.Fatalf("expected 1 channel, got %d", pcm.Channels)
	}
}

func TestFFmpegDecoderPCMA(t *testing.T) {
	dec := NewFFmpegDecoder("pcm_alaw")
	defer dec.Close()
	payload := make([]byte, 160)
	for i := range payload {
		payload[i] = 0xD5
	}
	pcm, err := dec.Decode(payload)
	if err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if len(pcm.Samples) != 160 {
		t.Fatalf("expected 160 samples, got %d", len(pcm.Samples))
	}
}

func TestFFmpegDecoderSetExtradataNoOp(t *testing.T) {
	dec := NewFFmpegDecoder("pcm_mulaw")
	defer dec.Close()
	dec.SetExtradata([]byte{0x12, 0x10})
}

// TestFFmpegDecoderAAC depends on NewFFmpegEncoder — tested after Task 5.
func TestFFmpegDecoderAAC(t *testing.T) {
	enc := NewFFmpegEncoder("aac", 44100, 2)
	defer enc.Close()
	pcm := &PCMFrame{Samples: make([]int16, 1024*2), SampleRate: 44100, Channels: 2}
	aacPayload, err := enc.Encode(pcm)
	if err != nil {
		t.Fatalf("encode AAC: %v", err)
	}
	dec := NewFFmpegDecoder("aac")
	defer dec.Close()
	dec.SetExtradata([]byte{0x12, 0x10})
	decoded, err := dec.Decode(aacPayload)
	if err != nil {
		t.Fatalf("decode AAC: %v", err)
	}
	if decoded.SampleRate != 44100 {
		t.Fatalf("expected 44100, got %d", decoded.SampleRate)
	}
	if decoded.Channels != 2 {
		t.Fatalf("expected 2 channels, got %d", decoded.Channels)
	}
	if len(decoded.Samples) < 1024 {
		t.Fatalf("expected at least 1024 samples, got %d", len(decoded.Samples))
	}
}
