package metrics

import (
	"fmt"
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

// TestPerStreamGaugesCreated verifies that per-stream gauges (bitrate, fps, uptime,
// gop_cache_frames) are emitted for each active stream.
func TestPerStreamGaugesCreated(t *testing.T) {
	cfg := testConfig()
	s := core.NewServer(cfg)
	m := NewModule()
	s.RegisterModule(m)

	if err := s.Init(); err != nil {
		t.Fatalf("init failed: %v", err)
	}
	defer s.Shutdown()

	stream, err := s.StreamHub().GetOrCreate("live/gauges")
	if err != nil {
		t.Fatalf("create stream failed: %v", err)
	}

	pub := &stubPublisher{
		id:        "pub-gauges",
		mediaInfo: &avframe.MediaInfo{VideoCodec: avframe.CodecH264, AudioCodec: avframe.CodecAAC},
	}
	if err := stream.SetPublisher(pub); err != nil {
		t.Fatalf("set publisher failed: %v", err)
	}

	// Write a keyframe to populate GOP cache and stats
	stream.WriteFrame(&avframe.AVFrame{
		MediaType: avframe.MediaTypeVideo,
		FrameType: avframe.FrameTypeKeyframe,
		Codec:     avframe.CodecH264,
		DTS:       0, PTS: 0,
		Payload: make([]byte, 2000),
	})

	time.Sleep(10 * time.Millisecond)

	addr := m.Addr()
	resp, err := http.Get("http://" + addr.String() + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics failed: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	content := string(body)

	// Per-stream gauges that should be present
	expectedMetrics := []string{
		`liveforge_stream_bitrate_kbps{stream_key="live/gauges"}`,
		`liveforge_stream_fps{stream_key="live/gauges"}`,
		`liveforge_stream_uptime_seconds{stream_key="live/gauges"}`,
		`liveforge_stream_gop_cache_frames{stream_key="live/gauges"}`,
	}
	for _, metric := range expectedMetrics {
		if !strings.Contains(content, metric) {
			t.Errorf("missing per-stream gauge: %s", metric)
		}
	}
}

// TestCollectMultipleStreams verifies that metrics are emitted for multiple
// concurrently active streams.
func TestCollectMultipleStreams(t *testing.T) {
	cfg := testConfig()
	s := core.NewServer(cfg)
	m := NewModule()
	s.RegisterModule(m)

	if err := s.Init(); err != nil {
		t.Fatalf("init failed: %v", err)
	}
	defer s.Shutdown()

	streamKeys := []string{"live/stream_a", "live/stream_b", "live/stream_c"}
	for i, key := range streamKeys {
		stream, err := s.StreamHub().GetOrCreate(key)
		if err != nil {
			t.Fatalf("create stream %s failed: %v", key, err)
		}
		pub := &stubPublisher{
			id:        fmt.Sprintf("pub-%d", i),
			mediaInfo: &avframe.MediaInfo{VideoCodec: avframe.CodecH264, AudioCodec: avframe.CodecAAC},
		}
		if err := stream.SetPublisher(pub); err != nil {
			t.Fatalf("set publisher for %s failed: %v", key, err)
		}
		stream.WriteFrame(&avframe.AVFrame{
			MediaType: avframe.MediaTypeVideo,
			FrameType: avframe.FrameTypeKeyframe,
			Codec:     avframe.CodecH264,
			DTS:       int64(i * 1000), PTS: int64(i * 1000),
			Payload: make([]byte, 500*(i+1)),
		})
	}

	time.Sleep(10 * time.Millisecond)

	addr := m.Addr()
	resp, err := http.Get("http://" + addr.String() + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics failed: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	content := string(body)

	// Verify active stream count
	if !strings.Contains(content, "liveforge_server_streams_active 3") {
		t.Error("expected liveforge_server_streams_active to be 3")
	}

	// Verify per-stream metrics for each stream
	for _, key := range streamKeys {
		label := fmt.Sprintf(`stream_key="%s"`, key)
		if !strings.Contains(content, label) {
			t.Errorf("missing metrics for stream %s", key)
		}
	}
}

// TestCollectCleanupRemovedStreams verifies that after a stream is removed from the
// hub, its metrics are no longer emitted.
func TestCollectCleanupRemovedStreams(t *testing.T) {
	cfg := testConfig()
	s := core.NewServer(cfg)
	m := NewModule()
	s.RegisterModule(m)

	if err := s.Init(); err != nil {
		t.Fatalf("init failed: %v", err)
	}
	defer s.Shutdown()

	// Create two streams
	for _, key := range []string{"live/keep", "live/remove"} {
		stream, err := s.StreamHub().GetOrCreate(key)
		if err != nil {
			t.Fatalf("create stream %s failed: %v", key, err)
		}
		pub := &stubPublisher{
			id:        "pub-" + key,
			mediaInfo: &avframe.MediaInfo{VideoCodec: avframe.CodecH264},
		}
		if err := stream.SetPublisher(pub); err != nil {
			t.Fatalf("set publisher for %s failed: %v", key, err)
		}
		stream.WriteFrame(&avframe.AVFrame{
			MediaType: avframe.MediaTypeVideo,
			FrameType: avframe.FrameTypeKeyframe,
			Codec:     avframe.CodecH264,
			DTS:       0, PTS: 0,
			Payload: make([]byte, 500),
		})
	}

	time.Sleep(10 * time.Millisecond)

	// Verify both streams appear before removal
	addr := m.Addr()
	resp1, err := http.Get("http://" + addr.String() + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics failed: %v", err)
	}
	body1, _ := io.ReadAll(resp1.Body)
	resp1.Body.Close()
	content1 := string(body1)

	if !strings.Contains(content1, `stream_key="live/keep"`) {
		t.Error("live/keep should be present before removal")
	}
	if !strings.Contains(content1, `stream_key="live/remove"`) {
		t.Error("live/remove should be present before removal")
	}

	// Remove the stream from the hub
	s.StreamHub().Remove("live/remove")

	time.Sleep(10 * time.Millisecond)

	// Verify only the kept stream remains
	resp2, err := http.Get("http://" + addr.String() + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics failed after removal: %v", err)
	}
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	content2 := string(body2)

	if !strings.Contains(content2, `stream_key="live/keep"`) {
		t.Error("live/keep should still be present after removing live/remove")
	}
	if strings.Contains(content2, `stream_key="live/remove"`) {
		t.Error("live/remove should not appear after being removed from the hub")
	}

	// Stream count should now be 1
	if !strings.Contains(content2, "liveforge_server_streams_active 1") {
		t.Error("expected liveforge_server_streams_active to be 1 after removal")
	}
}
