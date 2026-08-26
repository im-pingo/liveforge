package record

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
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

	firstFile := strings.TrimSuffix(w.FilePath(), ".partial")

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
	currentPartial := w.FilePath()
	if strings.TrimSuffix(currentPartial, ".partial") == firstFile {
		t.Error("expected file rotation due to max_size, but file path didn't change")
	}

	// Both files should exist
	if _, err := os.Stat(firstFile); err != nil {
		t.Errorf("first file should exist: %v", err)
	}
	if _, err := os.Stat(currentPartial); err != nil {
		t.Errorf("current rotated partial file should exist: %v", err)
	}
}

func TestNewFrameWriterFLV(t *testing.T) {
	w := newFrameWriter("flv")
	if _, ok := w.(*flvFrameWriter); !ok {
		t.Errorf("expected flvFrameWriter, got %T", w)
	}

	// New recordings default to fMP4 so audio/video playback uses one browser-friendly format.
	w = newFrameWriter("")
	if _, ok := w.(*fmp4FrameWriter); !ok {
		t.Errorf("default should be fmp4FrameWriter, got %T", w)
	}
}

func TestNewFrameWriterFMP4(t *testing.T) {
	w := newFrameWriter("fmp4")
	if _, ok := w.(*fmp4FrameWriter); !ok {
		t.Errorf("expected fmp4FrameWriter, got %T", w)
	}

	w = newFrameWriter("mp4")
	if _, ok := w.(*mp4FrameWriter); !ok {
		t.Errorf("mp4 should map to mp4FrameWriter, got %T", w)
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

func TestFileWriterExpandsConfiguredExtensionPlaceholder(t *testing.T) {
	tests := []struct {
		format string
		ext    string
	}{
		{format: "", ext: ".mp4"},
		{format: "flv", ext: ".flv"},
		{format: "mp4", ext: ".mp4"},
		{format: "fmp4", ext: ".mp4"},
		{format: "ts", ext: ".ts"},
		{format: "hls", ext: ".ts"},
	}
	for _, test := range tests {
		t.Run(test.format, func(t *testing.T) {
			writer, err := NewFileWriter("live/ext", config.RecordConfig{
				Format: test.format,
				Path:   filepath.Join(t.TempDir(), "{stream_key}.{ext}"),
			})
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Close()
			if got := writer.RecordingID(); !strings.HasSuffix(got, test.ext) || strings.Contains(got, "{ext}") {
				t.Fatalf("recording id = %q, want suffix %q", got, test.ext)
			}
		})
	}
}

func TestFileWriterNormalizesKnownPathExtensionToFormat(t *testing.T) {
	writer, err := NewFileWriter("live/mismatch", config.RecordConfig{
		Format: "fmp4",
		Path:   filepath.Join(t.TempDir(), "{stream_key}.flv"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if got := filepath.Ext(writer.RecordingID()); got != ".mp4" {
		t.Fatalf("recording extension = %q, want .mp4", got)
	}
}

func TestFileCompleteCallbackFailureLogsRedactedEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	callback, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	server.Close()
	callback.User = url.UserPassword("callback-user", "callback-password")
	callback.Path = "/callback-path-credential/signature-value"
	callback.RawQuery = "token=secret-query-value&signature=query-signature"

	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	writer, err := NewFileWriter("live/callback", config.RecordConfig{
		Format:         "flv",
		Path:           filepath.Join(t.TempDir(), "{stream_key}.flv"),
		OnFileComplete: config.FileCompleteConfig{URL: callback.String()},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe,
		0, 0, []byte{0x65, 0x01},
	)); err != nil {
		t.Fatal(err)
	}
	writer.Close()

	output := logs.String()
	if !strings.Contains(output, server.URL) {
		t.Fatalf("sanitized callback origin missing from log: %s", output)
	}
	for _, secret := range []string{"callback-user", "callback-password", "callback-path-credential", "signature-value", "secret-query-value", "query-signature", "token="} {
		if strings.Contains(output, secret) {
			t.Fatalf("callback log exposed %q: %s", secret, output)
		}
	}
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

func TestFileWriterRejectsSequenceHeaderOnlyRecording(t *testing.T) {
	local, err := NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	w, err := newFileWriterWithStorage("live/empty", config.RecordConfig{Format: "fmp4"}, local, "live/empty.mp4", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader,
		0, 0, []byte{0x01, 0x64, 0x00, 0x28},
	)); err != nil {
		t.Fatal(err)
	}
	if err := w.CloseWithError(nil); err == nil || !strings.Contains(err.Error(), "no media frames") {
		t.Fatalf("close error = %v, want no-media error", err)
	}
	items, err := local.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].State != RecordingFailed || items[0].Size != 0 {
		t.Fatalf("sequence-only recording = %+v, want one failed zero-byte artifact", items)
	}
}

func TestFileWriterDoesNotRotateOnSequenceHeader(t *testing.T) {
	local, err := NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	w, err := newFileWriterWithStorage("live/header", config.RecordConfig{
		Format:  "ts",
		Segment: config.SegmentConfig{Duration: time.Millisecond},
	}, local, "live/header.ts", nil)
	if err != nil {
		t.Fatal(err)
	}
	w.startTime = time.Now().Add(-time.Second)
	if err := w.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader,
		0, 0, []byte{0x01, 0x64, 0x00, 0x28},
	)); err != nil {
		t.Fatalf("sequence header should not rotate an empty segment: %v", err)
	}
	if w.segmentIndex != 0 {
		t.Fatalf("sequence header rotated segment: index=%d", w.segmentIndex)
	}
	if err := w.CloseWithError(ErrRecordingNoMedia); !errors.Is(err, ErrRecordingNoMedia) {
		t.Fatalf("close error = %v, want no-media error", err)
	}
}

func TestFMP4WriterWaitsForAllKnownSequenceHeaders(t *testing.T) {
	dir := t.TempDir()
	cfg := config.RecordConfig{
		Format: "fmp4",
		Path:   filepath.Join(dir, "{stream_key}", "{date}_{time}.mp4"),
	}
	w, err := NewFileWriter("live/av", cfg)
	if err != nil {
		t.Fatal(err)
	}

	videoSeq := avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader,
		0, 0, []byte{0x01, 0x64, 0x00, 0x28, 0xff, 0xe1, 0x00, 0x04, 0x67, 0x64, 0x00, 0x28, 0x01, 0x00, 0x04, 0x68, 0xee, 0x3c, 0x80},
	)
	audioSeq := avframe.NewAVFrame(
		avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeSequenceHeader,
		0, 0, []byte{0x12, 0x10},
	)
	if err := w.WriteFrame(videoSeq); err != nil {
		t.Fatal(err)
	}
	if writer, ok := w.format.(*fmp4FrameWriter); !ok || writer.initDone {
		t.Fatalf("fMP4 initialized before audio sequence header: %#v", w.format)
	}
	if err := w.WriteFrame(audioSeq); err != nil {
		t.Fatal(err)
	}
	if writer := w.format.(*fmp4FrameWriter); writer.initDone {
		t.Fatal("fMP4 initialized on sequence header; media must trigger init")
	}
	if err := w.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe,
		0, 0, []byte{0x65, 0x88, 0x00, 0x01},
	)); err != nil {
		t.Fatal(err)
	}
	if writer := w.format.(*fmp4FrameWriter); !writer.initDone {
		t.Fatal("fMP4 did not initialize when media arrived")
	}
	w.Close()
	data, err := os.ReadFile(w.FilePath())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("mp4a")) {
		t.Fatal("fMP4 init segment does not contain the AAC track")
	}
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

func TestFMP4RecordSessionIncludesLateAudioTrack(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		Record: config.RecordConfig{
			Enabled:       true,
			StreamPattern: "*",
			Format:        "fmp4",
			Path:          filepath.Join(dir, "{stream_key}", "{date}_{time}.mp4"),
		},
		Stream: config.StreamConfig{GOPCache: true, GOPCacheNum: 1, RingBufferSize: 256},
	}
	server := core.NewServer(cfg)
	stream, err := server.StreamHub().GetOrCreate("live/late-audio")
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.SetPublisher(&testPublisher{
		id:   "pub-late-audio",
		info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264, AudioCodec: avframe.CodecAAC},
	}); err != nil {
		t.Fatal(err)
	}
	session, err := NewRecordSession("live/late-audio", stream, cfg.Record)
	if err != nil {
		t.Fatal(err)
	}
	videoSeq := avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader,
		0, 0, []byte{0x01, 0x64, 0x00, 0x28, 0xff, 0xe1, 0x00, 0x04, 0x67, 0x64, 0x00, 0x28, 0x01, 0x00, 0x04, 0x68, 0xee, 0x3c, 0x80},
	)
	stream.WriteFrame(videoSeq)
	done := make(chan struct{})
	go func() {
		session.Run()
		close(done)
	}()

	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe,
		0, 0, []byte{0x65, 0x88, 0x00, 0x01},
	))
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe,
		33, 33, []byte{0x41, 0x9a, 0x00, 0x01},
	))
	deadline := time.Now().Add(2 * time.Second)
	for session.writer.BytesWritten() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if session.writer.BytesWritten() == 0 {
		t.Fatal("video-only fMP4 init segment was not written")
	}
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeSequenceHeader,
		0, 0, []byte{0x12, 0x10},
	))
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe,
		0, 0, []byte{0xff, 0xf1, 0x50, 0x80},
	))
	session.Stop()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("late-audio recording session did not stop")
	}

	data, err := os.ReadFile(session.writer.FilePath())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("mp4a")) {
		t.Fatal("fMP4 recording omitted the late AAC track")
	}
}

func TestModuleName(t *testing.T) {
	m := NewModule()
	if m.Name() != "record" {
		t.Errorf("expected name 'record', got %q", m.Name())
	}
}

func TestModuleOnPublishAndStop(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		Record: config.RecordConfig{
			Enabled:       true,
			StreamPattern: "live/*",
			Format:        "flv",
			Path:          filepath.Join(dir, "{stream_key}", "{date}_{time}.flv"),
		},
		Stream: config.StreamConfig{
			GOPCache:       true,
			GOPCacheNum:    1,
			RingBufferSize: 256,
		},
	}
	s := core.NewServer(cfg)

	// Create a stream and set publisher
	stream, _ := s.StreamHub().GetOrCreate("live/rec-hook")
	pub := &testPublisher{id: "pub-hook", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}
	_ = stream.SetPublisher(pub)

	// Write a frame so the recorder has data to read
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe,
		0, 0, []byte{0x65, 0x01},
	))

	m := NewModule()
	if err := m.Init(s); err != nil {
		t.Fatal(err)
	}

	// Trigger onPublish
	ctx := &core.EventContext{StreamKey: "live/rec-hook"}
	hooks := m.Hooks()
	// First hook is EventPublish
	if err := hooks[0].Handler(ctx); err != nil {
		t.Fatalf("onPublish: %v", err)
	}

	// Verify session exists
	m.mu.Lock()
	_, exists := m.sessions["live/rec-hook"]
	m.mu.Unlock()
	if !exists {
		t.Error("expected recording session to be created")
	}

	// Duplicate publish should be no-op
	if err := hooks[0].Handler(ctx); err != nil {
		t.Fatalf("duplicate onPublish: %v", err)
	}

	// Non-matching stream should be skipped
	ctx2 := &core.EventContext{StreamKey: "other/stream"}
	if err := hooks[0].Handler(ctx2); err != nil {
		t.Fatalf("non-matching onPublish: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	// Trigger onPublishStop
	if err := hooks[1].Handler(ctx); err != nil {
		t.Fatalf("onPublishStop: %v", err)
	}

	m.mu.Lock()
	_, exists = m.sessions["live/rec-hook"]
	m.mu.Unlock()
	if exists {
		t.Error("expected recording session to be removed after stop")
	}

	// Stop for non-existent session should be no-op
	if err := hooks[1].Handler(ctx); err != nil {
		t.Fatalf("second onPublishStop: %v", err)
	}

	m.Close()
}

func TestFileWriterDurationSegmentation(t *testing.T) {
	dir := t.TempDir()
	cfg := config.RecordConfig{
		Path: filepath.Join(dir, "{stream_key}", "{date}_{time}.flv"),
		Segment: config.SegmentConfig{
			Duration: 50 * time.Millisecond, // very short for testing
		},
	}

	w, err := NewFileWriter("live/seg", cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	firstFile := w.FilePath()

	// Write a frame
	frame := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 0, 0, []byte{0x65, 0x01})
	_ = w.WriteFrame(frame)

	// Wait for duration to exceed
	time.Sleep(60 * time.Millisecond)

	// Write another frame to trigger rotation
	frame2 := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 33, 33, []byte{0x65, 0x02})
	_ = w.WriteFrame(frame2)

	if w.FilePath() == firstFile {
		t.Error("expected file rotation due to duration, but file path didn't change")
	}
}

func TestFileWriterDefaultPath(t *testing.T) {
	// Test expandPath with empty Path (uses default)
	cfg := config.RecordConfig{
		Path:   "",
		Format: "flv",
	}
	w := &FileWriter{
		cfg:       cfg,
		streamKey: "live/test",
		format:    newFrameWriter("flv"),
	}
	p := w.expandPath()
	if p == "" {
		t.Error("expandPath should produce non-empty path even with empty config")
	}
}

func TestFileWriterDefaultPathFMP4(t *testing.T) {
	cfg := config.RecordConfig{
		Path:   "",
		Format: "fmp4",
	}
	w := &FileWriter{
		cfg:       cfg,
		streamKey: "live/test",
		format:    newFrameWriter("fmp4"),
	}
	p := w.expandPath()
	if p == "" {
		t.Error("expandPath should produce non-empty path")
	}
}

func TestNotifyFileComplete(t *testing.T) {
	// Test with empty URL (should be no-op)
	w := &FileWriter{
		cfg:       config.RecordConfig{},
		streamKey: "live/test",
		filePath:  "/tmp/test.flv",
		format:    newFrameWriter("flv"),
	}
	// Should not panic
	w.notifyFileComplete()
}

func TestNewFrameWriterTS(t *testing.T) {
	w := newFrameWriter("ts")
	if _, ok := w.(*tsFrameWriter); !ok {
		t.Errorf("expected tsFrameWriter, got %T", w)
	}
	w = newFrameWriter("hls")
	if _, ok := w.(*tsFrameWriter); !ok {
		t.Errorf("hls should map to tsFrameWriter, got %T", w)
	}
}

func TestLocalStorageListOmitsTSPlaylistArtifacts(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "live", "ts")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{
		"record.ts":        []byte("primary"),
		"segment_00000.ts": []byte("segment"),
		"index.m3u8":       []byte("#EXTM3U"),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	storage, err := NewLocalStorage(root)
	if err != nil {
		t.Fatal(err)
	}
	items, err := storage.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != "live/ts/record.ts" {
		t.Fatalf("playlist artifacts leaked into recording list: %+v", items)
	}
}

func TestTSRecordingCreatesSegmentsAndPlaylist(t *testing.T) {
	dir := t.TempDir()
	cfg := config.RecordConfig{
		Format: "ts",
		Path:   filepath.Join(dir, "{stream_key}", "{date}_{time}.ts"),
		Segment: config.SegmentConfig{
			Duration: 100 * time.Millisecond,
		},
	}

	w, err := NewFileWriter("live/tstest", cfg)
	if err != nil {
		t.Fatal(err)
	}

	// Write H.264 sequence header
	seqHeader := avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader,
		0, 0, []byte{0x01, 0x64, 0x00, 0x28, 0xFF, 0xE1, 0x00, 0x04, 0x67, 0x64, 0x00, 0x28, 0x01, 0x00, 0x04, 0x68, 0xEE, 0x3C, 0x80},
	)
	if err := w.WriteFrame(seqHeader); err != nil {
		t.Fatalf("write seq header: %v", err)
	}

	// Write first GOP: keyframe + interframes
	keyframe1 := avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe,
		0, 0, []byte{0x00, 0x00, 0x00, 0x02, 0x65, 0x88},
	)
	if err := w.WriteFrame(keyframe1); err != nil {
		t.Fatalf("write keyframe1: %v", err)
	}

	for i := 1; i <= 5; i++ {
		inter := avframe.NewAVFrame(
			avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe,
			int64(i*33), int64(i*33), []byte{0x00, 0x00, 0x00, 0x02, 0x41, 0x9A},
		)
		if err := w.WriteFrame(inter); err != nil {
			t.Fatalf("write interframe %d: %v", i, err)
		}
	}

	// Write second GOP to trigger segment split
	keyframe2 := avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe,
		200, 200, []byte{0x00, 0x00, 0x00, 0x02, 0x65, 0x89},
	)
	if err := w.WriteFrame(keyframe2); err != nil {
		t.Fatalf("write keyframe2: %v", err)
	}

	for i := 1; i <= 3; i++ {
		inter := avframe.NewAVFrame(
			avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe,
			int64(200+i*33), int64(200+i*33), []byte{0x00, 0x00, 0x00, 0x02, 0x41, 0x9B},
		)
		if err := w.WriteFrame(inter); err != nil {
			t.Fatalf("write interframe2 %d: %v", i, err)
		}
	}

	w.Close()
	mainInfo, err := os.Stat(w.FilePath())
	if err != nil {
		t.Fatalf("stat primary TS recording: %v", err)
	}
	if mainInfo.Size() == 0 {
		t.Fatal("primary TS recording is a zero-byte placeholder")
	}

	// Verify: the TS writer should have created segment files and a playlist
	parentDir := filepath.Dir(w.FilePath())
	entries, err := os.ReadDir(parentDir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}

	var tsFiles, m3u8Files []string
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".ts" {
			tsFiles = append(tsFiles, e.Name())
		}
		if filepath.Ext(e.Name()) == ".m3u8" {
			m3u8Files = append(m3u8Files, e.Name())
		}
	}

	if len(tsFiles) == 0 {
		t.Error("expected at least one .ts segment file")
	}
	if len(m3u8Files) == 0 {
		t.Error("expected index.m3u8 playlist file")
	}

	// Read playlist and verify VOD markers
	if len(m3u8Files) > 0 {
		playlist, err := os.ReadFile(filepath.Join(parentDir, m3u8Files[0]))
		if err != nil {
			t.Fatalf("read playlist: %v", err)
		}
		content := string(playlist)
		if !strings.Contains(content, "#EXTM3U") {
			t.Error("playlist missing #EXTM3U header")
		}
		if !strings.Contains(content, "#EXT-X-PLAYLIST-TYPE:VOD") {
			t.Error("playlist missing VOD type")
		}
		if !strings.Contains(content, "#EXT-X-ENDLIST") {
			t.Error("playlist missing #EXT-X-ENDLIST")
		}
		t.Logf("TS segments: %v, playlist:\n%s", tsFiles, content)
	}
}

func TestFileWriterFormatTS(t *testing.T) {
	dir := t.TempDir()
	cfg := config.RecordConfig{
		Format: "ts",
		Path:   filepath.Join(dir, "{stream_key}", "{date}_{time}.ts"),
	}
	w, err := NewFileWriter("live/test", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if w.Format() != "ts" {
		t.Errorf("Format = %q, want ts", w.Format())
	}
	w.Close()
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
