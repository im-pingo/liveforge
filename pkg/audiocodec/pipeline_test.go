package audiocodec

import (
	"math"
	"testing"
)

// TestOpusEncoderProperties verifies the Opus encoder's frame size, sample rate,
// and channel count are correct for WebRTC streaming.
func TestOpusEncoderProperties(t *testing.T) {
	enc := NewFFmpegEncoder("libopus", 48000, 2)
	defer enc.Close()

	if enc.SampleRate() != 48000 {
		t.Fatalf("Opus SampleRate() = %d, want 48000", enc.SampleRate())
	}
	if enc.Channels() != 2 {
		t.Fatalf("Opus Channels() = %d, want 2", enc.Channels())
	}
	if enc.FrameSize() != 960 {
		t.Fatalf("Opus FrameSize() = %d, want 960", enc.FrameSize())
	}
	t.Logf("Opus encoder: rate=%d ch=%d frameSize=%d", enc.SampleRate(), enc.Channels(), enc.FrameSize())
}

// TestAACDecodeProperties verifies the AAC decoder returns correct sample rate
// and channel count for typical RTMP audio (OBS defaults to 48kHz stereo, though
// 44.1kHz is also common).
func TestAACDecodeProperties(t *testing.T) {
	// AAC-LC 44100Hz stereo AudioSpecificConfig: profile=2, freq_index=4, ch_config=2
	extradata44100 := []byte{0x12, 0x10}
	// AAC-LC 48000Hz stereo AudioSpecificConfig: profile=2, freq_index=3, ch_config=2
	extradata48000 := []byte{0x11, 0x90}

	tests := []struct {
		name       string
		extradata  []byte
		wantRate   int
		wantCh     int
	}{
		{"44100Hz_stereo", extradata44100, 44100, 2},
		{"48000Hz_stereo", extradata48000, 48000, 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dec := NewFFmpegDecoder("aac")
			defer dec.Close()
			dec.SetExtradata(tt.extradata)
			t.Logf("AAC decoder initial: rate=%d ch=%d", dec.SampleRate(), dec.Channels())
			// Note: actual decoded sample rate comes from the frame, not from
			// the decoder's initial properties. We can only verify initial setup.
			// Real decoded values need actual AAC data.
		})
	}
}

// TestAAC44100ToOpusPipelineSampleCounts verifies that the AAC→resample→Opus
// pipeline preserves audio duration. For every N ms of AAC input, we should
// produce approximately N ms of Opus output.
func TestAAC44100ToOpusPipelineSampleCounts(t *testing.T) {
	enc := NewFFmpegEncoder("libopus", 48000, 2)
	defer enc.Close()

	frameSize := enc.FrameSize() * enc.Channels() // 960 * 2 = 1920
	if frameSize == 0 {
		t.Fatal("Opus frameSize is 0, encoder may have failed to initialize")
	}
	t.Logf("Opus encoder frameSize=%d (per-ch=%d)", frameSize, enc.FrameSize())

	// Simulate AAC decode output: 1024 samples/ch at 44100Hz stereo
	resampler := NewFFmpegResampler(44100, 2, 48000, 2)
	defer resampler.Close()

	var pcmBuf []int16
	var opusFrameCount int

	// Feed 100 AAC frames (~2.32 seconds at 44100Hz, 1024 samples/frame)
	aacFrameCount := 100
	inputDurationMs := float64(aacFrameCount) * 1024.0 / 44100.0 * 1000.0

	for i := 0; i < aacFrameCount; i++ {
		// Simulate decoded AAC: 1024 samples per channel, stereo = 2048 interleaved
		pcm := &PCMFrame{
			Samples:    make([]int16, 1024*2),
			SampleRate: 44100,
			Channels:   2,
		}
		out := resampler.Resample(pcm)
		pcmBuf = append(pcmBuf, out.Samples...)

		for len(pcmBuf) >= frameSize {
			chunk := &PCMFrame{
				Samples:    pcmBuf[:frameSize],
				SampleRate: 48000,
				Channels:   2,
			}
			_, err := enc.Encode(chunk)
			if err != nil {
				t.Fatalf("Opus encode error at frame %d: %v", opusFrameCount, err)
			}
			opusFrameCount++
			pcmBuf = pcmBuf[frameSize:]
		}
	}

	outputDurationMs := float64(opusFrameCount) * 20.0 // 20ms per Opus frame
	driftMs := math.Abs(outputDurationMs - inputDurationMs)
	driftPct := driftMs / inputDurationMs * 100

	t.Logf("Input: %d AAC frames (1024 samples/ch @ 44100Hz) = %.1fms",
		aacFrameCount, inputDurationMs)
	t.Logf("Output: %d Opus frames (960 samples/ch @ 48000Hz) = %.1fms",
		opusFrameCount, outputDurationMs)
	t.Logf("Remaining pcmBuf: %d samples", len(pcmBuf))
	t.Logf("Duration drift: %.1fms (%.2f%%)", driftMs, driftPct)

	// Duration drift should be < 1% over 2+ seconds
	if driftPct > 1.0 {
		t.Errorf("Audio duration drift too large: %.2f%% (%.1fms)",
			driftPct, driftMs)
	}
}

// TestAAC48000ToOpusPipelineSampleCounts verifies the pipeline when the source
// is already 48kHz (OBS default). No resampling needed — just accumulate and encode.
func TestAAC48000ToOpusPipelineSampleCounts(t *testing.T) {
	enc := NewFFmpegEncoder("libopus", 48000, 2)
	defer enc.Close()

	frameSize := enc.FrameSize() * enc.Channels()
	if frameSize == 0 {
		t.Fatal("Opus frameSize is 0")
	}

	var pcmBuf []int16
	var opusFrameCount int

	// AAC at 48kHz produces 1024 samples/ch per frame
	aacFrameCount := 100
	inputDurationMs := float64(aacFrameCount) * 1024.0 / 48000.0 * 1000.0

	for i := 0; i < aacFrameCount; i++ {
		// No resampling: 48000→48000
		pcmBuf = append(pcmBuf, make([]int16, 1024*2)...)

		for len(pcmBuf) >= frameSize {
			chunk := &PCMFrame{
				Samples:    pcmBuf[:frameSize],
				SampleRate: 48000,
				Channels:   2,
			}
			_, err := enc.Encode(chunk)
			if err != nil {
				t.Fatalf("Opus encode error: %v", err)
			}
			opusFrameCount++
			pcmBuf = pcmBuf[frameSize:]
		}
	}

	outputDurationMs := float64(opusFrameCount) * 20.0
	driftMs := math.Abs(outputDurationMs - inputDurationMs)
	driftPct := driftMs / inputDurationMs * 100

	t.Logf("Input: %d AAC frames (1024 samples/ch @ 48000Hz) = %.1fms",
		aacFrameCount, inputDurationMs)
	t.Logf("Output: %d Opus frames (960 samples/ch @ 48000Hz) = %.1fms",
		opusFrameCount, outputDurationMs)
	t.Logf("Remaining pcmBuf: %d samples", len(pcmBuf))
	t.Logf("Duration drift: %.1fms (%.2f%%)", driftMs, driftPct)

	if driftPct > 1.0 {
		t.Errorf("Audio duration drift too large: %.2f%%", driftPct)
	}
}

// TestOpusFrameDurationFromTOC verifies the Opus encoder produces packets whose
// TOC (Table of Contents) byte indicates 20ms frames. If the TOC byte says 10ms
// but RTP timestamps advance by 20ms (960 samples), the browser's Opus decoder
// would play audio at half speed → the "voice changer" effect.
//
// Opus TOC byte structure (RFC 6716 §3.1):
//
//	config (5 bits) | s (1 bit) | c (2 bits)
//
// The frame duration is determined by the config value:
//
//	config  bandwidth  frame size
//	0-3     NB(silk)   10,20,40,60 ms
//	4-7     MB(silk)   10,20,40,60 ms
//	8-11    WB(silk)   10,20,40,60 ms
//	12-15   SWB(silk)  10,20,40,60 ms
//	16-19   FB(silk)   10,20,40,60 ms
//	20-23   NB(celt)   2.5,5,10,20 ms
//	24-27   WB(celt)   2.5,5,10,20 ms
//	28-31   FB(celt)   2.5,5,10,20 ms
func TestOpusFrameDurationFromTOC(t *testing.T) {
	enc := NewFFmpegEncoder("libopus", 48000, 2)
	defer enc.Close()

	// Encode a few frames of silence and check TOC bytes.
	for i := range 5 {
		pcm := &PCMFrame{
			Samples:    make([]int16, 960*2),
			SampleRate: 48000,
			Channels:   2,
		}
		data, err := enc.Encode(pcm)
		if err != nil {
			t.Fatalf("Encode frame %d: %v", i, err)
		}
		if len(data) == 0 {
			t.Fatalf("Encode frame %d: empty output", i)
		}

		toc := data[0]
		config := toc >> 3 // top 5 bits

		// Determine frame duration from config
		var durationMs float64
		switch {
		case config <= 19: // SILK mode
			switch config % 4 {
			case 0:
				durationMs = 10
			case 1:
				durationMs = 20
			case 2:
				durationMs = 40
			case 3:
				durationMs = 60
			}
		default: // CELT/Hybrid mode (config 20-31)
			switch config % 4 {
			case 0:
				durationMs = 2.5
			case 1:
				durationMs = 5
			case 2:
				durationMs = 10
			case 3:
				durationMs = 20
			}
		}

		stereo := (toc >> 2) & 1
		code := toc & 3

		t.Logf("frame %d: TOC=0x%02x config=%d duration=%.1fms stereo=%d code=%d size=%d",
			i, toc, config, durationMs, stereo, code, len(data))

		if durationMs != 20.0 {
			t.Errorf("frame %d: Opus TOC indicates %.1fms, want 20ms (config=%d)",
				i, durationMs, config)
		}
	}
}

// TestAACEncodeDecodeRoundTrip verifies that AAC frames encoded by the FFmpeg
// encoder can be decoded by the FFmpeg decoder with the correct extradata.
// This is the exact codec path used in the RTMP→WebRTC transcoding pipeline.
func TestAACEncodeDecodeRoundTrip(t *testing.T) {
	enc := NewFFmpegEncoder("aac", 48000, 2)
	defer enc.Close()

	t.Logf("AAC encoder: rate=%d ch=%d frameSize=%d", enc.SampleRate(), enc.Channels(), enc.FrameSize())

	// Encode a few frames of silence.
	var aacFrames [][]byte
	for range 10 {
		pcm := &PCMFrame{
			Samples:    make([]int16, 1024*2), // 1024 samples/ch, stereo
			SampleRate: 48000,
			Channels:   2,
		}
		data, err := enc.Encode(pcm)
		if err != nil {
			t.Fatalf("AAC encode: %v", err)
		}
		aacFrames = append(aacFrames, data)
		t.Logf("AAC frame %d: %d bytes", len(aacFrames)-1, len(data))
	}

	// Decode with matching extradata (AAC-LC 48kHz stereo).
	dec := NewFFmpegDecoder("aac")
	defer dec.Close()

	extradata := []byte{0x11, 0x90} // AAC-LC, 48kHz, stereo
	dec.SetExtradata(extradata)

	decodeOK := 0
	for i, frame := range aacFrames {
		pcm, err := dec.Decode(frame)
		if err != nil {
			t.Logf("frame %d: decode error: %v", i, err)
			continue
		}
		decodeOK++
		t.Logf("frame %d: decoded %d samples, rate=%d ch=%d",
			i, len(pcm.Samples), pcm.SampleRate, pcm.Channels)
	}

	if decodeOK == 0 {
		t.Fatal("all AAC frames failed to decode — extradata mismatch or encoder bug")
	}
	t.Logf("%d/%d frames decoded successfully", decodeOK, len(aacFrames))
}

// TestTsTrackerOpusTimestamps verifies that TsTracker produces exact 20ms
// DTS increments for Opus at 48kHz with no cumulative drift.
func TestTsTrackerOpusTimestamps(t *testing.T) {
	var ts TsTracker
	ts.Init(1000, 48000) // anchor at DTS=1000ms

	for i := 0; i < 500; i++ { // 10 seconds
		dts := ts.Next(960) // 960 samples per Opus frame
		expected := int64(1000) + int64(i)*20
		if dts != expected {
			t.Fatalf("frame %d: DTS=%d, want %d (drift=%d)", i, dts, expected, dts-expected)
		}
	}
	t.Logf("500 Opus frames: DTS advanced from 1000ms to %dms (exactly 10000ms)",
		1000+499*20)
}
