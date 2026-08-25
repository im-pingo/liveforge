package dvr

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
)

func TestSegmentIndex_AddAndLookup(t *testing.T) {
	idx := NewSegmentIndex()

	now := time.Now()
	for i := 0; i < 5; i++ {
		idx.Add(Segment{
			SeqNum:    i,
			StartTime: now.Add(time.Duration(i) * 6 * time.Second),
			Duration:  6.0,
			Filename:  segFilename(i),
			DiskPath:  "/tmp/test/" + segFilename(i),
		})
	}

	if idx.Len() != 5 {
		t.Fatalf("Len = %d, want 5", idx.Len())
	}

	seg, ok := idx.SegmentBySeqNum(3)
	if !ok {
		t.Fatal("SegmentBySeqNum(3) not found")
	}
	if seg.SeqNum != 3 {
		t.Errorf("SeqNum = %d, want 3", seg.SeqNum)
	}

	_, ok = idx.SegmentBySeqNum(99)
	if ok {
		t.Error("SegmentBySeqNum(99) should not be found")
	}

	first, _ := idx.First()
	if first.SeqNum != 0 {
		t.Errorf("First.SeqNum = %d, want 0", first.SeqNum)
	}

	last, _ := idx.Last()
	if last.SeqNum != 4 {
		t.Errorf("Last.SeqNum = %d, want 4", last.SeqNum)
	}

	if idx.MaxDuration() != 6.0 {
		t.Errorf("MaxDuration = %f, want 6.0", idx.MaxDuration())
	}
}

func TestSegmentIndex_CleanBefore(t *testing.T) {
	dir := t.TempDir()
	idx := NewSegmentIndex()

	now := time.Now()
	for i := 0; i < 5; i++ {
		filename := segFilename(i)
		path := filepath.Join(dir, filename)
		os.WriteFile(path, []byte("test data"), 0644)

		idx.Add(Segment{
			SeqNum:    i,
			StartTime: now.Add(time.Duration(i-3) * time.Hour),
			Duration:  6.0,
			Filename:  filename,
			DiskPath:  path,
		})
	}

	// Clean segments older than 1 hour
	cutoff := now.Add(-1 * time.Hour)
	removed := idx.CleanBefore(cutoff)

	if len(removed) != 2 {
		t.Fatalf("removed %d segments, want 2", len(removed))
	}

	if idx.Len() != 3 {
		t.Errorf("remaining = %d, want 3", idx.Len())
	}

	// Verify files were deleted
	for _, seg := range removed {
		if _, err := os.Stat(seg.DiskPath); !os.IsNotExist(err) {
			t.Errorf("file %s should have been deleted", seg.DiskPath)
		}
	}

	// Verify remaining files still exist
	segs := idx.Segments()
	for _, seg := range segs {
		if _, err := os.Stat(seg.DiskPath); err != nil {
			t.Errorf("file %s should still exist", seg.DiskPath)
		}
	}
}

func TestSegmentIndex_EmptyIndex(t *testing.T) {
	idx := NewSegmentIndex()

	if idx.Len() != 0 {
		t.Error("expected empty index")
	}

	_, ok := idx.First()
	if ok {
		t.Error("First on empty index should return false")
	}

	_, ok = idx.Last()
	if ok {
		t.Error("Last on empty index should return false")
	}

	if idx.MaxDuration() != 0 {
		t.Error("MaxDuration on empty index should be 0")
	}
}

func TestGeneratePlaylist_Live(t *testing.T) {
	idx := NewSegmentIndex()
	now := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)

	for i := 0; i < 3; i++ {
		idx.Add(Segment{
			SeqNum:    i,
			StartTime: now.Add(time.Duration(i) * 6 * time.Second),
			Duration:  6.0,
			Filename:  segFilename(i),
		})
	}

	playlist := GeneratePlaylist(idx, "live/mystream", true)

	if !strings.Contains(playlist, "#EXTM3U") {
		t.Error("missing #EXTM3U")
	}
	if !strings.Contains(playlist, "#EXT-X-VERSION:6") {
		t.Error("missing VERSION:6")
	}
	if !strings.Contains(playlist, "#EXT-X-MEDIA-SEQUENCE:0") {
		t.Error("missing MEDIA-SEQUENCE")
	}
	if !strings.Contains(playlist, "#EXT-X-PROGRAM-DATE-TIME:2026-05-25T12:00:00.000Z") {
		t.Error("missing PROGRAM-DATE-TIME for first segment")
	}
	if !strings.Contains(playlist, "#EXTINF:6.000,") {
		t.Error("missing EXTINF")
	}
	if !strings.Contains(playlist, "mystream/seg_000000.ts") {
		t.Error("missing segment URL")
	}
	if strings.Contains(playlist, "#EXT-X-ENDLIST") {
		t.Error("live playlist should not have ENDLIST")
	}
}

func TestGeneratePlaylist_Stopped(t *testing.T) {
	idx := NewSegmentIndex()
	now := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)

	idx.Add(Segment{
		SeqNum:    5,
		StartTime: now,
		Duration:  4.5,
		Filename:  segFilename(5),
	})

	playlist := GeneratePlaylist(idx, "live/test", false)

	if !strings.Contains(playlist, "#EXT-X-ENDLIST") {
		t.Error("stopped playlist should have ENDLIST")
	}
	if !strings.Contains(playlist, "#EXT-X-MEDIA-SEQUENCE:5") {
		t.Error("media sequence should be 5")
	}
}

func TestGeneratePlaylist_Empty(t *testing.T) {
	idx := NewSegmentIndex()
	playlist := GeneratePlaylist(idx, "live/test", true)
	if playlist != "" {
		t.Errorf("expected empty playlist, got %q", playlist)
	}
}

func TestSession_WritesSegments(t *testing.T) {
	dir := t.TempDir()
	bus := core.NewEventBus()
	hub := core.NewStreamHub(config.StreamConfig{RingBufferSize: 256}, config.LimitsConfig{}, bus)

	stream, _ := hub.GetOrCreate("live/dvr-test")

	cfg := config.DVRConfig{
		Enabled:         true,
		Path:            filepath.Join(dir, "{stream_key}"),
		Window:          2 * time.Hour,
		SegmentDuration: 100 * time.Millisecond,
		CleanupInterval: 30 * time.Second,
	}

	session, err := NewSession("live/dvr-test", stream, cfg, nil, 0)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	// Feed frames in a goroutine
	go func() {
		// Sequence headers
		stream.WriteFrame(&avframe.AVFrame{
			MediaType: avframe.MediaTypeVideo,
			Codec:     avframe.CodecH264,
			FrameType: avframe.FrameTypeSequenceHeader,
			DTS:       0,
			PTS:       0,
			Payload:   []byte{0x67, 0x42, 0x00, 0x1e, 0x67, 0x42, 0x00, 0x1e, 0x68, 0xce, 0x38, 0x80},
		})

		// Write 3 keyframes to trigger segment splits
		for i := 0; i < 3; i++ {
			dts := int64(i * 200)
			stream.WriteFrame(&avframe.AVFrame{
				MediaType: avframe.MediaTypeVideo,
				Codec:     avframe.CodecH264,
				FrameType: avframe.FrameTypeKeyframe,
				DTS:       dts,
				PTS:       dts,
				Payload:   []byte{0x65, 0x88, 0x84, 0x00, 0x33},
			})
			// Inter frame
			stream.WriteFrame(&avframe.AVFrame{
				MediaType: avframe.MediaTypeVideo,
				Codec:     avframe.CodecH264,
				FrameType: avframe.FrameTypeInterframe,
				DTS:       dts + 33,
				PTS:       dts + 33,
				Payload:   []byte{0x41, 0x9a, 0x24},
			})
		}

		// Give session time to process, then stop
		time.Sleep(50 * time.Millisecond)
		session.Stop()
	}()

	session.Run()

	// Verify segments were created
	segCount := session.Index().Len()
	if segCount < 2 {
		t.Errorf("expected at least 2 segments, got %d", segCount)
	}

	// Verify segment files exist on disk
	segs := session.Index().Segments()
	for _, seg := range segs {
		if _, err := os.Stat(seg.DiskPath); err != nil {
			t.Errorf("segment file missing: %s", seg.DiskPath)
		}
	}
}

func TestModuleHooks(t *testing.T) {
	m := NewModule()
	hooks := m.Hooks()

	if len(hooks) != 2 {
		t.Fatalf("expected 2 hooks, got %d", len(hooks))
	}

	if hooks[0].Event != core.EventPublish {
		t.Errorf("hook[0] event = %v, want EventPublish", hooks[0].Event)
	}
	if hooks[1].Event != core.EventPublishStop {
		t.Errorf("hook[1] event = %v, want EventPublishStop", hooks[1].Event)
	}
}

func TestModuleName(t *testing.T) {
	m := NewModule()
	if m.Name() != "dvr" {
		t.Errorf("Name = %q, want dvr", m.Name())
	}
}

func TestResolvePath(t *testing.T) {
	tests := []struct {
		template  string
		streamKey string
		want      string
	}{
		{"./dvr/{stream_key}", "live/test", "dvr/live/test"},
		{"/data/dvr/{stream_key}", "app/cam1", "/data/dvr/app/cam1"},
	}

	for _, tt := range tests {
		got := resolvePath(tt.template, tt.streamKey)
		if got != tt.want {
			t.Errorf("resolvePath(%q, %q) = %q, want %q", tt.template, tt.streamKey, got, tt.want)
		}
	}
}

func TestParseSeqNum(t *testing.T) {
	tests := []struct {
		filename string
		want     int
	}{
		{"seg_000000.ts", 0},
		{"seg_000042.ts", 42},
		{"seg_001234.ts", 1234},
		{"bad_name.ts", -1},
		{"seg_.ts", -1},
		{"seg_42.ts", -1},
		{"seg_abc.ts", -1},
	}

	for _, tt := range tests {
		got := parseSeqNum(tt.filename)
		if got != tt.want {
			t.Errorf("parseSeqNum(%q) = %d, want %d", tt.filename, got, tt.want)
		}
	}
}

func TestMatchPattern(t *testing.T) {
	tests := []struct {
		pattern string
		key     string
		want    bool
	}{
		{"", "anything", true},
		{"*", "anything", true},
		{"live/*", "live/test", true},
		{"live/*", "vod/test", false},
		{"live/cam*", "live/cam1", true},
	}

	for _, tt := range tests {
		got := matchPattern(tt.pattern, tt.key)
		if got != tt.want {
			t.Errorf("matchPattern(%q, %q) = %v, want %v", tt.pattern, tt.key, got, tt.want)
		}
	}
}

func segFilename(seq int) string {
	return "seg_" + padInt(seq) + ".ts"
}

func padInt(n int) string {
	s := "000000"
	ns := ""
	for n > 0 {
		ns = string(rune('0'+n%10)) + ns
		n /= 10
	}
	if ns == "" {
		ns = "0"
	}
	if len(ns) < 6 {
		return s[:6-len(ns)] + ns
	}
	return ns
}
