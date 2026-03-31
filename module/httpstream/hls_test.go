package httpstream

import (
	"bytes"
	"testing"

	"github.com/im-pingo/liveforge/pkg/avframe"
)

func TestHLSManagerGenerateM3U8(t *testing.T) {
	mgr := NewHLSManager("live/test", "/live/test", 6.0, 3)

	// Empty playlist
	m3u8 := mgr.GenerateM3U8()
	if m3u8 == "" {
		t.Error("expected non-empty playlist")
	}

	// Add segments
	mgr.mu.Lock()
	mgr.segments = []*HLSSegment{
		{SeqNum: 0, Duration: 5.5, Data: []byte("seg0")},
		{SeqNum: 1, Duration: 6.1, Data: []byte("seg1")},
	}
	mgr.nextSeqNum = 2
	mgr.mu.Unlock()

	m3u8 = mgr.GenerateM3U8()
	if m3u8 == "" {
		t.Error("expected non-empty playlist with segments")
	}
}

func TestHLSManagerGetSegment(t *testing.T) {
	mgr := NewHLSManager("live/test", "/live/test", 6.0, 3)

	mgr.mu.Lock()
	mgr.segments = []*HLSSegment{
		{SeqNum: 5, Duration: 6.0, Data: []byte("seg-5")},
		{SeqNum: 6, Duration: 6.0, Data: []byte("seg-6")},
	}
	mgr.seqBase = 5
	mgr.mu.Unlock()

	data, ok := mgr.GetSegment(5)
	if !ok || string(data) != "seg-5" {
		t.Errorf("GetSegment(5) = %q, %v", data, ok)
	}

	data, ok = mgr.GetSegment(6)
	if !ok || string(data) != "seg-6" {
		t.Errorf("GetSegment(6) = %q, %v", data, ok)
	}

	_, ok = mgr.GetSegment(99)
	if ok {
		t.Error("GetSegment(99) should not find")
	}
}

func TestHLSManagerSegmentCount(t *testing.T) {
	mgr := NewHLSManager("live/test", "/live/test", 6.0, 3)

	if mgr.SegmentCount() != 0 {
		t.Errorf("expected 0, got %d", mgr.SegmentCount())
	}

	mgr.mu.Lock()
	mgr.segments = []*HLSSegment{{SeqNum: 0}, {SeqNum: 1}}
	mgr.mu.Unlock()

	if mgr.SegmentCount() != 2 {
		t.Errorf("expected 2, got %d", mgr.SegmentCount())
	}
}

func TestCopyBytesAndBufCopyAndReset(t *testing.T) {
	src := []byte("hello world")
	dst := copyBytes(src)
	if string(dst) != "hello world" {
		t.Errorf("copyBytes: got %q", dst)
	}
	// Modify src, dst should be independent
	src[0] = 'H'
	if dst[0] == 'H' {
		t.Error("copyBytes should return independent copy")
	}
}

func TestParseAudioSeqHeader(t *testing.T) {
	// AAC-LC, 44100 Hz, stereo: first 2 bytes = 0x12 0x10
	// objectType=2 (AAC-LC), samplingFrequencyIndex=4 (44100), channelConfiguration=2
	payload := []byte{0x12, 0x10}
	sr, ch := parseAudioSeqHeader(&avframe.AVFrame{Payload: payload})
	if sr != 44100 {
		t.Errorf("sample rate: got %d, want 44100", sr)
	}
	if ch != 2 {
		t.Errorf("channels: got %d, want 2", ch)
	}

	// Invalid payload
	sr, ch = parseAudioSeqHeader(&avframe.AVFrame{Payload: []byte{0xFF}})
	if sr != 0 || ch != 0 {
		t.Errorf("invalid payload: got sr=%d ch=%d, want 0,0", sr, ch)
	}
}

func TestHLSManagerDefaults(t *testing.T) {
	mgr := NewHLSManager("test", "/test", 0, 0)
	// Should use defaults: targetDur=6.0, maxSegments=5
	mgr.mu.RLock()
	defer mgr.mu.RUnlock()
	if mgr.targetDur != 6.0 {
		t.Errorf("default targetDur: got %f, want 6.0", mgr.targetDur)
	}
	if mgr.maxSegments != 5 {
		t.Errorf("default maxSegments: got %d, want 5", mgr.maxSegments)
	}
}

func TestDASHManagerDefaults(t *testing.T) {
	mgr := NewDASHManager("test", "/test", 0, 0)
	mgr.mu.RLock()
	defer mgr.mu.RUnlock()
	if mgr.targetDur != 6.0 {
		t.Errorf("default targetDur: got %f, want 6.0", mgr.targetDur)
	}
	if mgr.maxSegments != 30 {
		t.Errorf("default maxSegments: got %d, want 30", mgr.maxSegments)
	}
}

func TestBufCopyAndReset(t *testing.T) {
	var buf bytes.Buffer
	buf.Write([]byte("test data"))
	result := bufCopyAndReset(&buf)
	if string(result) != "test data" {
		t.Errorf("got %q", result)
	}
	if buf.Len() != 0 {
		t.Error("buffer should be reset")
	}
}
