package analyzer

import (
	"math"
	"testing"

	"github.com/im-pingo/liveforge/pkg/avframe"
)

// buildSPS returns a minimal SPS NAL unit for 1920x1080 High profile, level 4.0.
func buildSPS() []byte {
	return []byte{
		0x67, 0x64, 0x00, 0x28, 0xAC, 0xD9, 0x40, 0x78,
		0x02, 0x27, 0xE5, 0xC0, 0x44, 0x00, 0x00, 0x03,
		0x00, 0x04, 0x00, 0x00, 0x03, 0x00, 0xF0, 0x3C,
		0x60, 0xC6, 0x58,
	}
}

// buildAACAudioSpecificConfig returns a 2-byte AudioSpecificConfig for 44100Hz stereo AAC-LC.
// ObjectType=2 (AAC-LC), FreqIndex=4 (44100), Channels=2
func buildAACConfig() []byte {
	// objectType=2 => 5 bits: 00010
	// freqIndex=4  => 4 bits: 0100
	// channels=2   => 4 bits: 0010
	// padding       => 3 bits: 000
	// byte0: 00010_010 = 0x12
	// byte1: 0_0010_000 = 0x10
	return []byte{0x12, 0x10}
}

func TestAnalyzerBasic(t *testing.T) {
	a := New()

	// Feed a video sequence header with real SPS data (Annex-B format).
	sps := buildSPS()
	annexB := append([]byte{0x00, 0x00, 0x00, 0x01}, sps...)
	a.Feed(avframe.NewAVFrame(
		avframe.MediaTypeVideo,
		avframe.CodecH264,
		avframe.FrameTypeSequenceHeader,
		0, 0,
		annexB,
	))

	// Feed an audio sequence header with AAC AudioSpecificConfig.
	a.Feed(avframe.NewAVFrame(
		avframe.MediaTypeAudio,
		avframe.CodecAAC,
		avframe.FrameTypeSequenceHeader,
		0, 0,
		buildAACConfig(),
	))

	// Feed 30 video frames at 30fps (one every ~33ms), starting at DTS=0.
	// Every 10th frame is a keyframe.
	for i := 0; i < 30; i++ {
		dts := int64(i * 33)
		ft := avframe.FrameTypeInterframe
		if i%10 == 0 {
			ft = avframe.FrameTypeKeyframe
		}
		payload := make([]byte, 5000) // ~5KB per frame
		a.Feed(avframe.NewAVFrame(
			avframe.MediaTypeVideo,
			avframe.CodecH264,
			ft,
			dts, dts,
			payload,
		))
	}

	// Feed 43 audio frames at ~23ms intervals (43.07 fps for AAC at 44100/1024).
	for i := 0; i < 43; i++ {
		dts := int64(i * 23)
		payload := make([]byte, 200) // ~200 bytes per audio frame
		a.Feed(avframe.NewAVFrame(
			avframe.MediaTypeAudio,
			avframe.CodecAAC,
			avframe.FrameTypeInterframe,
			dts, dts,
			payload,
		))
	}

	r := a.Report()

	// Verify video frame count.
	if r.Video.FrameCount != 30 {
		t.Errorf("video frame count: want 30, got %d", r.Video.FrameCount)
	}

	// Verify audio frame count.
	if r.Audio.FrameCount != 43 {
		t.Errorf("audio frame count: want 43, got %d", r.Audio.FrameCount)
	}

	// Verify video FPS is approximately 30.
	if r.Video.FPS < 25 || r.Video.FPS > 35 {
		t.Errorf("video FPS: want ~30, got %.2f", r.Video.FPS)
	}

	// Verify DTS monotonicity for both tracks.
	if !r.Video.DTSMonotonic {
		t.Error("video DTS should be monotonic")
	}
	if !r.Audio.DTSMonotonic {
		t.Error("audio DTS should be monotonic")
	}

	// Verify video bitrate is positive.
	if r.Video.BitrateKbps <= 0 {
		t.Errorf("video bitrate should be > 0, got %.2f", r.Video.BitrateKbps)
	}

	// Verify audio bitrate is positive.
	if r.Audio.BitrateKbps <= 0 {
		t.Errorf("audio bitrate should be > 0, got %.2f", r.Audio.BitrateKbps)
	}

	// Verify keyframe interval: 3 keyframes at DTS 0, 330, 660 → ~0.33s apart.
	if r.Video.KeyframeInterval <= 0 {
		t.Errorf("keyframe interval should be > 0, got %.2f", r.Video.KeyframeInterval)
	}

	// Verify codec report parsed from sequence headers.
	if r.Video.Codec != "H264" {
		t.Errorf("video codec: want H264, got %s", r.Video.Codec)
	}
	if r.Video.Resolution != "1920x1080" {
		t.Errorf("video resolution: want 1920x1080, got %s", r.Video.Resolution)
	}
	if r.Audio.Codec != "AAC" {
		t.Errorf("audio codec: want AAC, got %s", r.Audio.Codec)
	}
	if r.Audio.SampleRate != 44100 {
		t.Errorf("audio sample rate: want 44100, got %d", r.Audio.SampleRate)
	}
	if r.Audio.Channels != 2 {
		t.Errorf("audio channels: want 2, got %d", r.Audio.Channels)
	}

	// Verify sync report has reasonable drift (all frames start near DTS 0).
	if r.Sync.MaxDriftMs < 0 {
		t.Errorf("sync max drift should be >= 0, got %.2f", r.Sync.MaxDriftMs)
	}

	// Verify duration.
	if r.DurationMs <= 0 {
		t.Errorf("duration should be > 0, got %d", r.DurationMs)
	}

	// Verify no stalls in normal playback.
	if len(r.Stalls) != 0 {
		t.Errorf("expected 0 stalls, got %d", len(r.Stalls))
	}
}

func TestAnalyzerDTSNonMonotonic(t *testing.T) {
	a := New()

	// Feed video frames with a DTS regression at frame 3.
	timestamps := []int64{0, 33, 66, 50, 132}
	for _, dts := range timestamps {
		a.Feed(avframe.NewAVFrame(
			avframe.MediaTypeVideo,
			avframe.CodecH264,
			avframe.FrameTypeInterframe,
			dts, dts,
			make([]byte, 100),
		))
	}

	r := a.Report()
	if r.Video.DTSMonotonic {
		t.Error("video DTS should NOT be monotonic when a regression exists")
	}
}

func TestAnalyzerStallDetection(t *testing.T) {
	a := New()

	// Feed 20 video frames at 33ms intervals (normal 30fps).
	for i := 0; i < 20; i++ {
		dts := int64(i * 33)
		a.Feed(avframe.NewAVFrame(
			avframe.MediaTypeVideo,
			avframe.CodecH264,
			avframe.FrameTypeInterframe,
			dts, dts,
			make([]byte, 100),
		))
	}

	// Now insert a 5-second gap: the next frame jumps from DTS=627 to DTS=5627.
	a.Feed(avframe.NewAVFrame(
		avframe.MediaTypeVideo,
		avframe.CodecH264,
		avframe.FrameTypeInterframe,
		5627, 5627,
		make([]byte, 100),
	))

	// Feed a few more normal frames after the gap.
	for i := 1; i <= 5; i++ {
		dts := int64(5627 + i*33)
		a.Feed(avframe.NewAVFrame(
			avframe.MediaTypeVideo,
			avframe.CodecH264,
			avframe.FrameTypeInterframe,
			dts, dts,
			make([]byte, 100),
		))
	}

	r := a.Report()

	if len(r.Stalls) == 0 {
		t.Fatal("expected at least one stall event for 5s gap")
	}

	// The stall gap should be approximately 5000ms.
	found := false
	for _, s := range r.Stalls {
		if s.GapMs >= 4000 && s.GapMs <= 6000 {
			found = true
			if s.MediaType != "video" {
				t.Errorf("stall media type: want video, got %s", s.MediaType)
			}
		}
	}
	if !found {
		t.Errorf("no stall with ~5000ms gap found; stalls: %+v", r.Stalls)
	}
}

func TestAnalyzerSyncDrift(t *testing.T) {
	a := New()

	// Interleave video and audio with growing drift.
	// Video at 33ms intervals, audio starts 50ms behind and drifts further.
	for i := 0; i < 20; i++ {
		vDTS := int64(i * 33)
		aDTS := int64(i*33 + 50 + i*5) // growing drift: 50, 55, 60, ...
		a.Feed(avframe.NewAVFrame(
			avframe.MediaTypeVideo, avframe.CodecH264,
			avframe.FrameTypeInterframe, vDTS, vDTS, make([]byte, 100),
		))
		a.Feed(avframe.NewAVFrame(
			avframe.MediaTypeAudio, avframe.CodecAAC,
			avframe.FrameTypeInterframe, aDTS, aDTS, make([]byte, 100),
		))
	}

	r := a.Report()

	// Max drift should be around 145ms (50 + 19*5 = 145).
	if r.Sync.MaxDriftMs < 100 {
		t.Errorf("sync max drift too low: want >= 100, got %.2f", r.Sync.MaxDriftMs)
	}

	// Average drift should be positive and less than max.
	if r.Sync.AvgDriftMs <= 0 || r.Sync.AvgDriftMs > r.Sync.MaxDriftMs {
		t.Errorf("sync avg drift invalid: avg=%.2f max=%.2f", r.Sync.AvgDriftMs, r.Sync.MaxDriftMs)
	}
}

func TestAnalyzerEmptyReport(t *testing.T) {
	a := New()
	r := a.Report()

	// An empty analyzer should produce a valid zero report.
	if r.Video.FrameCount != 0 {
		t.Errorf("video frame count: want 0, got %d", r.Video.FrameCount)
	}
	if r.Audio.FrameCount != 0 {
		t.Errorf("audio frame count: want 0, got %d", r.Audio.FrameCount)
	}
	if r.DurationMs != 0 {
		t.Errorf("duration: want 0, got %d", r.DurationMs)
	}
	if len(r.Stalls) != 0 {
		t.Errorf("stalls: want 0, got %d", len(r.Stalls))
	}
}

func TestAnalyzerBitrateCalculation(t *testing.T) {
	a := New()

	// Feed 10 video frames: 1000 bytes each, over 1 second (100ms interval).
	for i := 0; i < 10; i++ {
		dts := int64(i * 100)
		a.Feed(avframe.NewAVFrame(
			avframe.MediaTypeVideo, avframe.CodecH264,
			avframe.FrameTypeInterframe, dts, dts,
			make([]byte, 1000),
		))
	}

	r := a.Report()

	// Total bytes = 10 * 1000 = 10000
	// Duration = 900ms (DTS 0 to 900)
	// Bitrate = 10000 * 8 / 900 ≈ 88.89 kbps
	expectedKbps := float64(10000*8) / float64(900)
	if math.Abs(r.Video.BitrateKbps-expectedKbps) > 1.0 {
		t.Errorf("video bitrate: want ~%.2f, got %.2f", expectedKbps, r.Video.BitrateKbps)
	}
}
