package dvr

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/muxer/ts"
)

type dvrStaleHistoryPublisher struct {
	id   string
	info *avframe.MediaInfo
}

func (p *dvrStaleHistoryPublisher) ID() string                    { return p.id }
func (p *dvrStaleHistoryPublisher) MediaInfo() *avframe.MediaInfo { return p.info }
func (p *dvrStaleHistoryPublisher) Close() error                  { return nil }

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

func TestSessionStaleHistoryStartsAtCurrentGeneration(t *testing.T) {
	dir := t.TempDir()
	stream := core.NewStream("live/stale-dvr", config.StreamConfig{
		GOPCache:       true,
		GOPCacheNum:    1,
		RingBufferSize: 32,
	}, config.LimitsConfig{}, core.NewEventBus())
	defer stream.Close()
	old := &dvrStaleHistoryPublisher{id: "publisher-a", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}
	if err := stream.SetPublisher(old); err != nil {
		t.Fatal(err)
	}
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader,
		0, 0, []byte{0x67, 0xaa},
	))
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe,
		0, 0, []byte{0, 0, 0, 3, 0x65, 0xaa, 0x01},
	))

	stream.RemovePublisher()
	current := &dvrStaleHistoryPublisher{id: "publisher-b", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}
	if err := stream.SetPublisher(current); err != nil {
		t.Fatal(err)
	}
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader,
		0, 0, []byte{0x67, 0xbb},
	))
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe,
		0, 0, []byte{0, 0, 0, 3, 0x65, 0xbb, 0x02},
	))

	session, err := NewSession("live/stale-dvr", stream, config.DVRConfig{
		Path: filepath.Join(dir, "{stream_key}"),
	}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe,
		33, 33, []byte{0, 0, 0, 3, 0x41, 0xbc, 0x03},
	))

	done := make(chan struct{})
	go func() {
		session.Run()
		close(done)
	}()
	time.Sleep(100 * time.Millisecond)
	session.Stop()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("DVR session did not stop")
	}

	var data []byte
	for _, segment := range session.Index().Segments() {
		segmentData, readErr := os.ReadFile(segment.DiskPath)
		if readErr != nil {
			t.Fatal(readErr)
		}
		data = append(data, segmentData...)
	}
	if bytes.Contains(data, []byte{0x65, 0xaa}) {
		t.Fatal("DVR contains publisher-A media from retained ring history")
	}
	if !bytes.Contains(data, []byte{0x65, 0xbb}) || !bytes.Contains(data, []byte{0x41, 0xbc}) {
		t.Fatalf("DVR does not contain publisher-B replay and live media: %x", data)
	}
}

func TestSessionDrainsFramesWrittenAfterSnapshotWhenPublisherGenerationEnds(t *testing.T) {
	dir := t.TempDir()
	stream := core.NewStream("live/dvr-generation-tail", config.StreamConfig{
		GOPCache:       true,
		GOPCacheNum:    1,
		RingBufferSize: 32,
	}, config.LimitsConfig{}, core.NewEventBus())
	defer stream.Close()
	pub := &dvrStaleHistoryPublisher{id: "generation-tail", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}
	if err := stream.SetPublisher(pub); err != nil {
		t.Fatal(err)
	}
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader,
		0, 0, dvrTestAVCConfig(),
	))
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe,
		0, 0, []byte{0, 0, 0, 4, 0x65, 0x11, 0x22, 0x33},
	))

	session, err := NewSession("live/dvr-generation-tail", stream, config.DVRConfig{
		Path: filepath.Join(dir, "{stream_key}"),
	}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	tailMarker := []byte{0xde, 0xad, 0xbe, 0xef}
	tailPayload := append([]byte{0, 0, 0, 5, 0x41}, tailMarker...)
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe,
		33, 33, tailPayload,
	))
	stream.RemovePublisherIf(pub)
	session.Run()

	segments := session.Index().Segments()
	if len(segments) != 1 {
		t.Fatalf("segments after generation tail drain = %d, want 1", len(segments))
	}
	data, err := os.ReadFile(segments[0].DiskPath)
	if err != nil {
		t.Fatal(err)
	}
	gotTail := false
	demuxer := ts.NewDemuxer(func(frame *avframe.AVFrame) {
		if frame.MediaType.IsVideo() && bytes.Contains(frame.Payload, tailMarker) {
			gotTail = true
		}
	})
	demuxer.Feed(data)
	demuxer.Flush()
	if !gotTail {
		t.Fatalf("DVR segment omitted generation tail marker %x", tailMarker)
	}
}

func TestDVRWaitsForLateVideoHeaderBeforeInitializingMuxer(t *testing.T) {
	dir := t.TempDir()
	stream := core.NewStream("live/dvr-late-header", config.StreamConfig{
		GOPCache:       true,
		GOPCacheNum:    1,
		RingBufferSize: 32,
	}, config.LimitsConfig{}, core.NewEventBus())
	defer stream.Close()
	publisher := &dvrStaleHistoryPublisher{id: "dvr-late-header", info: &avframe.MediaInfo{
		VideoCodec: avframe.CodecH264,
	}}
	if err := stream.SetPublisher(publisher); err != nil {
		t.Fatal(err)
	}
	session, err := NewSession("live/dvr-late-header", stream, config.DVRConfig{
		Path:            filepath.Join(dir, "{stream_key}"),
		SegmentDuration: time.Second,
	}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	done := make(chan struct{})
	go func() {
		session.Run()
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe,
		0, 0, []byte{0, 0, 0, 2, 0x65, 0x01},
	))
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader,
		0, 0, dvrTestAVCConfig(),
	))
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe,
		33, 33, []byte{0, 0, 0, 2, 0x41, 0x02},
	))
	time.Sleep(50 * time.Millisecond)
	session.Stop()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("DVR session did not stop")
	}

	var gotSequenceHeader bool
	for _, segment := range session.Index().Segments() {
		data, err := os.ReadFile(segment.DiskPath)
		if err != nil {
			t.Fatal(err)
		}
		demuxer := ts.NewDemuxer(func(frame *avframe.AVFrame) {
			if frame.MediaType.IsVideo() && frame.FrameType == avframe.FrameTypeSequenceHeader {
				gotSequenceHeader = true
			}
		})
		demuxer.Feed(data)
		demuxer.Flush()
	}
	if !gotSequenceHeader {
		t.Fatal("DVR first segment has no video sequence header after a late header")
	}
}

func dvrTestAVCConfig() []byte {
	sps := []byte{0x67, 0x42, 0x00, 0x1f, 0xe9, 0x40, 0x14, 0x04, 0x78}
	pps := []byte{0x68, 0xce, 0x38, 0x80}
	config := []byte{1, sps[1], sps[2], sps[3], 0xff, 0xe1, 0, byte(len(sps))}
	config = append(config, sps...)
	config = append(config, 1, 0, byte(len(pps)))
	return append(config, pps...)
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
