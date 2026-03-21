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
