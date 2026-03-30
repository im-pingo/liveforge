package record

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
)

func newTestConfig(dir string) *config.Config {
	return &config.Config{
		Record: config.RecordConfig{
			Enabled:       true,
			StreamPattern: "*",
			Format:        "flv",
			Path:          filepath.Join(dir, "{stream_key}", "{date}_{time}.flv"),
		},
		Stream: config.StreamConfig{
			GOPCache:           true,
			GOPCacheNum:        1,
			AudioCacheMs:       1000,
			RingBufferSize:     256,
			IdleTimeout:        5 * time.Second,
			NoPublisherTimeout: 3 * time.Second,
		},
	}
}

type testPublisher struct {
	id   string
	info *avframe.MediaInfo
}

func (p *testPublisher) ID() string                    { return p.id }
func (p *testPublisher) MediaInfo() *avframe.MediaInfo { return p.info }
func (p *testPublisher) Close() error                  { return nil }

func TestMatchPattern(t *testing.T) {
	tests := []struct {
		pattern string
		key     string
		want    bool
	}{
		{"*", "live/test", true},
		{"", "anything", true},
		{"live/*", "live/test", true},
		{"live/*", "other/test", false},
		{"live/test", "live/test", true},
		{"live/test", "live/other", false},
	}
	for _, tt := range tests {
		got := matchPattern(tt.pattern, tt.key)
		if got != tt.want {
			t.Errorf("matchPattern(%q, %q) = %v, want %v", tt.pattern, tt.key, got, tt.want)
		}
	}
}

func TestFileWriterCreatesFile(t *testing.T) {
	dir := t.TempDir()
	cfg := config.RecordConfig{
		Path: filepath.Join(dir, "{stream_key}", "{date}_{time}.flv"),
	}

	w, err := NewFileWriter("live/test", cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	if w.FilePath() == "" {
		t.Error("expected non-empty file path")
	}

	// Write a video frame
	frame := avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe,
		0, 0, []byte{0x00, 0x01, 0x02, 0x03},
	)
	if err := w.WriteFrame(frame); err != nil {
		t.Fatalf("write frame: %v", err)
	}

	w.Close()

	// Verify file exists and has content
	info, err := os.Stat(w.FilePath())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() == 0 {
		t.Error("expected non-empty file")
	}
}

func TestRecordSessionEndToEnd(t *testing.T) {
	dir := t.TempDir()
	cfg := newTestConfig(dir)
	s := core.NewServer(cfg)

	hub := s.StreamHub()
	stream, err := hub.GetOrCreate("live/rec")
	if err != nil {
		t.Fatal(err)
	}
	pub := &testPublisher{
		id:   "pub-rec",
		info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264},
	}
	if err := stream.SetPublisher(pub); err != nil {
		t.Fatal(err)
	}

	session, err := NewRecordSession("live/rec", stream, cfg.Record)
	if err != nil {
		t.Fatal(err)
	}

	// Run session in background
	done := make(chan struct{})
	go func() {
		session.Run()
		close(done)
	}()

	// Write some frames
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe,
		0, 0, []byte{0x00, 0x01, 0x02, 0x03},
	))
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe,
		33, 33, []byte{0x04, 0x05},
	))
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe,
		66, 66, []byte{0x06, 0x07},
	))

	time.Sleep(100 * time.Millisecond)
	session.Stop()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("session did not stop in time")
	}

	// Verify file was created
	filePath := session.writer.FilePath()
	info, err := os.Stat(filePath)
	if err != nil {
		t.Fatalf("file not created: %v", err)
	}
	if info.Size() == 0 {
		t.Error("expected non-empty recording file")
	}
	t.Logf("recorded %d bytes to %s", info.Size(), filePath)
}

func TestParseSize(t *testing.T) {
	tests := []struct {
		input string
		want  int64
	}{
		{"512MB", 512 * 1024 * 1024},
		{"1GB", 1024 * 1024 * 1024},
		{"100KB", 100 * 1024},
		{"1024B", 1024},
		{"0MB", 0},
		{"", 0},
		{"invalid", 0},
		{"  256mb  ", 256 * 1024 * 1024}, // case insensitive + whitespace
	}
	for _, tt := range tests {
		got := parseSize(tt.input)
		if got != tt.want {
			t.Errorf("parseSize(%q) = %d, want %d", tt.input, got, tt.want)
		}
	}
}

func TestFileWriterMaxSizeSegmentation(t *testing.T) {
	dir := t.TempDir()
	cfg := config.RecordConfig{
		Path: filepath.Join(dir, "{stream_key}", "{date}_{time}.flv"),
		Segment: config.SegmentConfig{
			MaxSize: "1KB", // very small to trigger rotation
		},
	}

	w, err := NewFileWriter("live/test", cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	firstFile := w.FilePath()

	// Write frames until we exceed 1KB
	for i := 0; i < 20; i++ {
		frame := avframe.NewAVFrame(
			avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe,
			int64(i*33), int64(i*33), make([]byte, 200),
		)
		if err := w.WriteFrame(frame); err != nil {
			t.Fatalf("write frame %d: %v", i, err)
		}
	}

	// File should have rotated — current path should differ from first
	if w.FilePath() == firstFile {
		t.Error("expected file rotation due to max_size, but file path didn't change")
	}

	// Both files should exist
	if _, err := os.Stat(firstFile); err != nil {
		t.Errorf("first file should exist: %v", err)
	}
	if _, err := os.Stat(w.FilePath()); err != nil {
		t.Errorf("rotated file should exist: %v", err)
	}
}

func TestNewFrameWriterFLV(t *testing.T) {
	w := newFrameWriter("flv")
	if _, ok := w.(*flvFrameWriter); !ok {
		t.Errorf("expected flvFrameWriter, got %T", w)
	}

	// Default should also be FLV
	w = newFrameWriter("")
	if _, ok := w.(*flvFrameWriter); !ok {
		t.Errorf("default should be flvFrameWriter, got %T", w)
	}
}

func TestNewFrameWriterFMP4(t *testing.T) {
	w := newFrameWriter("fmp4")
	if _, ok := w.(*fmp4FrameWriter); !ok {
		t.Errorf("expected fmp4FrameWriter, got %T", w)
	}

	w = newFrameWriter("mp4")
	if _, ok := w.(*fmp4FrameWriter); !ok {
		t.Errorf("mp4 should map to fmp4FrameWriter, got %T", w)
	}
}

func TestFileWriterFormat(t *testing.T) {
	dir := t.TempDir()

	// FLV
	cfg := config.RecordConfig{
		Format: "flv",
		Path:   filepath.Join(dir, "flv_{time}.flv"),
	}
	w, err := NewFileWriter("live/test", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if w.Format() != "flv" {
		t.Errorf("Format = %q, want flv", w.Format())
	}
	w.Close()

	// fMP4
	cfg.Format = "fmp4"
	cfg.Path = filepath.Join(dir, "fmp4_{time}.mp4")
	w, err = NewFileWriter("live/test", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if w.Format() != "fmp4" {
		t.Errorf("Format = %q, want fmp4", w.Format())
	}
	w.Close()
}

func TestFMP4FileWriterCreatesFile(t *testing.T) {
	dir := t.TempDir()
	cfg := config.RecordConfig{
		Format: "fmp4",
		Path:   filepath.Join(dir, "{stream_key}", "{date}_{time}.mp4"),
	}

	w, err := NewFileWriter("live/test", cfg)
	if err != nil {
		t.Fatal(err)
	}

	// Write sequence header first (required for fMP4 init segment)
	seqHeader := avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader,
		0, 0, []byte{0x01, 0x64, 0x00, 0x28, 0xFF, 0xE1, 0x00, 0x04, 0x67, 0x64, 0x00, 0x28, 0x01, 0x00, 0x04, 0x68, 0xEE, 0x3C, 0x80},
	)
	if err := w.WriteFrame(seqHeader); err != nil {
		t.Fatalf("write seq header: %v", err)
	}

	// Write keyframe
	keyframe := avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe,
		0, 0, []byte{0x65, 0x88, 0x00, 0x01},
	)
	if err := w.WriteFrame(keyframe); err != nil {
		t.Fatalf("write keyframe: %v", err)
	}

	// Write interframes
	for i := 1; i <= 3; i++ {
		interframe := avframe.NewAVFrame(
			avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe,
			int64(i*33), int64(i*33), []byte{0x41, 0x9A, 0x00, 0x01},
		)
		if err := w.WriteFrame(interframe); err != nil {
			t.Fatalf("write interframe %d: %v", i, err)
		}
	}

	// Write another keyframe to trigger segment flush
	keyframe2 := avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe,
		132, 132, []byte{0x65, 0x88, 0x00, 0x02},
	)
	if err := w.WriteFrame(keyframe2); err != nil {
		t.Fatalf("write keyframe2: %v", err)
	}

	w.Close()

	// Verify file exists and has content (init segment + at least one media segment)
	info, err := os.Stat(w.FilePath())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() == 0 {
		t.Error("expected non-empty fMP4 file")
	}
	t.Logf("fMP4 recorded %d bytes to %s", info.Size(), w.FilePath())
}

func TestFMP4RecordSessionEndToEnd(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		Record: config.RecordConfig{
			Enabled:       true,
			StreamPattern: "*",
			Format:        "fmp4",
			Path:          filepath.Join(dir, "{stream_key}", "{date}_{time}.mp4"),
		},
		Stream: config.StreamConfig{
			GOPCache:           true,
			GOPCacheNum:        1,
			AudioCacheMs:       1000,
			RingBufferSize:     256,
			IdleTimeout:        5 * time.Second,
			NoPublisherTimeout: 3 * time.Second,
		},
	}

	s := core.NewServer(cfg)
	hub := s.StreamHub()
	stream, err := hub.GetOrCreate("live/fmp4rec")
	if err != nil {
		t.Fatal(err)
	}
	pub := &testPublisher{
		id:   "pub-fmp4",
		info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264},
	}
	if err := stream.SetPublisher(pub); err != nil {
		t.Fatal(err)
	}

	session, err := NewRecordSession("live/fmp4rec", stream, cfg.Record)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		session.Run()
		close(done)
	}()

	// Write sequence header + keyframes + interframes
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader,
		0, 0, []byte{0x01, 0x64, 0x00, 0x28, 0xFF, 0xE1, 0x00, 0x04, 0x67, 0x64, 0x00, 0x28, 0x01, 0x00, 0x04, 0x68, 0xEE, 0x3C, 0x80},
	))
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe,
		0, 0, []byte{0x65, 0x88, 0x00, 0x01},
	))
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe,
		33, 33, []byte{0x41, 0x9A, 0x00, 0x01},
	))
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe,
		66, 66, []byte{0x65, 0x88, 0x00, 0x02},
	))

	time.Sleep(100 * time.Millisecond)
	session.Stop()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("session did not stop in time")
	}

	filePath := session.writer.FilePath()
	info, err := os.Stat(filePath)
	if err != nil {
		t.Fatalf("file not created: %v", err)
	}
	if info.Size() == 0 {
		t.Error("expected non-empty fMP4 recording file")
	}
	t.Logf("fMP4 recorded %d bytes to %s", info.Size(), filePath)
}

func TestModuleHooks(t *testing.T) {
	dir := t.TempDir()
	cfg := newTestConfig(dir)
	s := core.NewServer(cfg)

	m := NewModule()
	if err := m.Init(s); err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	hooks := m.Hooks()
	if len(hooks) != 2 {
		t.Fatalf("expected 2 hooks, got %d", len(hooks))
	}
	for _, h := range hooks {
		if h.Mode != core.HookAsync {
			t.Errorf("expected async hook, got %v", h.Mode)
		}
		if h.Priority != 50 {
			t.Errorf("expected priority 50, got %d", h.Priority)
		}
	}
}
