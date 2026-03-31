package httpstream

import (
	"strings"
	"testing"

	"github.com/im-pingo/liveforge/pkg/avframe"
)

func TestDASHVideoCodecString(t *testing.T) {
	tests := []struct {
		name   string
		codec  avframe.CodecType
		seq    *avframe.AVFrame
		want   string
	}{
		{"h264 with header", avframe.CodecH264, &avframe.AVFrame{Payload: []byte{0x01, 0x64, 0x00, 0x28}}, "avc1.640028"},
		{"h264 fallback", avframe.CodecH264, nil, "avc1.640028"},
		{"h265 fallback", avframe.CodecH265, nil, "hvc1.1.6.L120.B0"},
		{"unknown codec", avframe.CodecType(99), nil, "avc1.640028"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dashVideoCodecString(tt.codec, tt.seq)
			if got != tt.want {
				t.Errorf("dashVideoCodecString() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDASHAudioCodecString(t *testing.T) {
	tests := []struct {
		name  string
		codec avframe.CodecType
		seq   *avframe.AVFrame
		want  string
	}{
		{"aac default", avframe.CodecAAC, nil, "mp4a.40.2"},
		{"opus", avframe.CodecOpus, nil, "opus"},
		{"mp3", avframe.CodecMP3, nil, "mp4a.40.34"},
		{"unknown", avframe.CodecType(99), nil, "mp4a.40.2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dashAudioCodecString(tt.codec, tt.seq)
			if got != tt.want {
				t.Errorf("dashAudioCodecString() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDASHManagerSegmentRange(t *testing.T) {
	mgr := NewDASHManager("test", "/test", 6.0, 5)

	// Empty case
	lo, hi := mgr.SegmentRange()
	if lo != -1 || hi != -1 {
		t.Errorf("empty: lo=%d, hi=%d, want -1, -1", lo, hi)
	}

	// Add segments
	mgr.mu.Lock()
	mgr.videoSegments = []*DASHSegment{
		{SeqNum: 3, Duration: 2.0, Data: []byte("a")},
		{SeqNum: 4, Duration: 2.0, Data: []byte("b")},
		{SeqNum: 5, Duration: 2.0, Data: []byte("c")},
	}
	mgr.mu.Unlock()

	lo, hi = mgr.SegmentRange()
	if lo != 3 || hi != 5 {
		t.Errorf("with segments: lo=%d, hi=%d, want 3, 5", lo, hi)
	}
}

func TestDASHManagerGetSegmentAndAudioSegment(t *testing.T) {
	mgr := NewDASHManager("test", "/test", 6.0, 5)

	mgr.mu.Lock()
	mgr.videoSegments = []*DASHSegment{{SeqNum: 0, Data: []byte("vid")}}
	mgr.audioSegments = []*DASHSegment{{SeqNum: 0, Data: []byte("aud")}}
	mgr.videoInitSeg = []byte("vinit")
	mgr.audioInitSeg = []byte("ainit")
	mgr.mu.Unlock()

	// Video segment
	data, ok := mgr.GetSegment(0)
	if !ok || string(data) != "vid" {
		t.Errorf("GetSegment(0) = %q, %v", data, ok)
	}
	_, ok = mgr.GetSegment(99)
	if ok {
		t.Error("GetSegment(99) should not find")
	}

	// Audio segment
	data, ok = mgr.GetAudioSegment(0)
	if !ok || string(data) != "aud" {
		t.Errorf("GetAudioSegment(0) = %q, %v", data, ok)
	}

	// Init segments
	data, ok = mgr.GetInitSegment()
	if !ok || string(data) != "vinit" {
		t.Errorf("GetInitSegment() = %q, %v", data, ok)
	}
	data, ok = mgr.GetAudioInitSegment()
	if !ok || string(data) != "ainit" {
		t.Errorf("GetAudioInitSegment() = %q, %v", data, ok)
	}
}

func TestDASHManagerGenerateMPD(t *testing.T) {
	mgr := NewDASHManager("live/test", "/live/test", 6.0, 5)

	mgr.mu.Lock()
	mgr.videoCodecStr = "avc1.640028"
	mgr.videoWidth = 1920
	mgr.videoHeight = 1080
	mgr.hasAudio = true
	mgr.audioCodec = "mp4a.40.2"
	mgr.audioSampleRate = 44100
	mgr.videoSegments = []*DASHSegment{
		{SeqNum: 0, Duration: 6.0, Data: []byte("seg0")},
		{SeqNum: 1, Duration: 6.0, Data: []byte("seg1")},
	}
	mgr.audioSegments = []*DASHSegment{
		{SeqNum: 0, Duration: 6.0, Data: []byte("aseg0")},
		{SeqNum: 1, Duration: 6.0, Data: []byte("aseg1")},
	}
	mgr.nextSeqNum = 2
	mgr.mu.Unlock()

	mpd := mgr.GenerateMPD()
	if mpd == "" {
		t.Fatal("expected non-empty MPD")
	}
	if !strings.Contains(mpd, "avc1.640028") {
		t.Error("MPD should contain video codec string")
	}
	if !strings.Contains(mpd, "mp4a.40.2") {
		t.Error("MPD should contain audio codec string")
	}
	if !strings.Contains(mpd, "1920") {
		t.Error("MPD should contain video width")
	}
}

func TestDASHManagerStopIdempotent(t *testing.T) {
	mgr := NewDASHManager("live/test", "/live/test", 6.0, 5)
	mgr.Stop()
	mgr.Stop() // should not panic
}

func TestDASHManagerGenerateMPDVideoOnly(t *testing.T) {
	mgr := NewDASHManager("live/test", "/live/test", 6.0, 5)

	mgr.mu.Lock()
	mgr.videoCodecStr = "avc1.640028"
	mgr.videoWidth = 1280
	mgr.videoHeight = 720
	mgr.hasAudio = false
	mgr.videoSegments = []*DASHSegment{
		{SeqNum: 0, Duration: 6.0, Data: []byte("seg0")},
	}
	mgr.nextSeqNum = 1
	mgr.mu.Unlock()

	mpd := mgr.GenerateMPD()
	if !strings.Contains(mpd, "avc1.640028") {
		t.Error("MPD should contain video codec")
	}
	if strings.Contains(mpd, "mp4a") {
		t.Error("video-only MPD should not contain audio codec")
	}
}

func TestDASHManagerEmptyInitSegment(t *testing.T) {
	mgr := NewDASHManager("live/test", "/live/test", 6.0, 5)

	_, ok := mgr.GetInitSegment()
	if ok {
		t.Error("empty manager should not have init segment")
	}
	_, ok = mgr.GetAudioInitSegment()
	if ok {
		t.Error("empty manager should not have audio init segment")
	}
}
