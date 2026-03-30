package metrics

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
)

func testConfig() *config.Config {
	return &config.Config{
		Stream: config.StreamConfig{
			GOPCache:       true,
			GOPCacheNum:    1,
			AudioCacheMs:   1000,
			RingBufferSize: 1024,
		},
		Metrics: config.MetricsConfig{
			Enabled: true,
			Listen:  ":0", // random port
			Path:    "/metrics",
		},
	}
}

func TestMetricsModuleStartStop(t *testing.T) {
	cfg := testConfig()
	s := core.NewServer(cfg)
	m := NewModule()
	s.RegisterModule(m)

	if err := s.Init(); err != nil {
		t.Fatalf("init failed: %v", err)
	}
	defer s.Shutdown()

	addr := m.Addr()
	if addr == nil {
		t.Fatal("expected listener address")
	}

	resp, err := http.Get("http://" + addr.String() + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	content := string(body)

	// Verify server-level metrics are present
	if !strings.Contains(content, "liveforge_server_streams_active") {
		t.Error("missing liveforge_server_streams_active metric")
	}
	if !strings.Contains(content, "liveforge_server_connections_active") {
		t.Error("missing liveforge_server_connections_active metric")
	}
	if !strings.Contains(content, "liveforge_server_uptime_seconds") {
		t.Error("missing liveforge_server_uptime_seconds metric")
	}
}

type stubPublisher struct {
	id        string
	mediaInfo *avframe.MediaInfo
}

func (p *stubPublisher) ID() string                 { return p.id }
func (p *stubPublisher) MediaInfo() *avframe.MediaInfo { return p.mediaInfo }
func (p *stubPublisher) Close() error                { return nil }

func TestMetricsWithStreams(t *testing.T) {
	cfg := testConfig()
	s := core.NewServer(cfg)
	m := NewModule()
	s.RegisterModule(m)

	if err := s.Init(); err != nil {
		t.Fatalf("init failed: %v", err)
	}
	defer s.Shutdown()

	// Create a stream with a publisher
	stream, err := s.StreamHub().GetOrCreate("live/test")
	if err != nil {
		t.Fatalf("create stream failed: %v", err)
	}

	pub := &stubPublisher{
		id: "rtmp-pub-1",
		mediaInfo: &avframe.MediaInfo{
			VideoCodec: avframe.CodecH264,
			AudioCodec: avframe.CodecAAC,
		},
	}
	if err := stream.SetPublisher(pub); err != nil {
		t.Fatalf("set publisher failed: %v", err)
	}

	// Write some frames to generate stats
	videoFrame := &avframe.AVFrame{
		MediaType: avframe.MediaTypeVideo,
		FrameType: avframe.FrameTypeKeyframe,
		Codec:     avframe.CodecH264,
		DTS:       0,
		PTS:       0,
		Payload:   make([]byte, 1000),
	}
	stream.WriteFrame(videoFrame)

	audioFrame := &avframe.AVFrame{
		MediaType: avframe.MediaTypeAudio,
		FrameType: avframe.FrameTypeInterframe,
		Codec:     avframe.CodecAAC,
		DTS:       0,
		PTS:       0,
		Payload:   make([]byte, 200),
	}
	stream.WriteFrame(audioFrame)

	// Give a moment for stats to propagate
	time.Sleep(10 * time.Millisecond)

	addr := m.Addr()
	resp, err := http.Get("http://" + addr.String() + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics failed: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	content := string(body)

	// Verify stream-level metrics
	if !strings.Contains(content, `liveforge_stream_bytes_in_total{stream_key="live/test"}`) {
		t.Error("missing liveforge_stream_bytes_in_total for live/test")
	}
	if !strings.Contains(content, `liveforge_stream_video_frames_total{stream_key="live/test"}`) {
		t.Error("missing liveforge_stream_video_frames_total for live/test")
	}
	if !strings.Contains(content, `liveforge_stream_audio_frames_total{stream_key="live/test"}`) {
		t.Error("missing liveforge_stream_audio_frames_total for live/test")
	}
	if !strings.Contains(content, `liveforge_stream_gop_cache_frames{stream_key="live/test"}`) {
		t.Error("missing liveforge_stream_gop_cache_frames for live/test")
	}

	// Verify Go runtime metrics are present
	if !strings.Contains(content, "go_goroutines") {
		t.Error("missing go_goroutines metric")
	}
}

func TestMetricsModuleName(t *testing.T) {
	m := NewModule()
	if m.Name() != "metrics" {
		t.Errorf("expected name 'metrics', got '%s'", m.Name())
	}
}

func TestMetricsDefaultPath(t *testing.T) {
	cfg := testConfig()
	cfg.Metrics.Path = "" // empty should default to /metrics
	s := core.NewServer(cfg)
	m := NewModule()
	s.RegisterModule(m)

	if err := s.Init(); err != nil {
		t.Fatalf("init failed: %v", err)
	}
	defer s.Shutdown()

	addr := m.Addr()
	resp, err := http.Get("http://" + addr.String() + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}
