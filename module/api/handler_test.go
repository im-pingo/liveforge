package api

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	configruntime "github.com/im-pingo/liveforge/config/runtime"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/module/dvr"
	"github.com/im-pingo/liveforge/pkg/audiocodec"
	"github.com/im-pingo/liveforge/pkg/avframe"
)

type endpointTestModule struct {
	name string
	addr net.Addr
}

func (m *endpointTestModule) Name() string                   { return m.name }
func (m *endpointTestModule) Init(*core.Server) error        { return nil }
func (m *endpointTestModule) Hooks() []core.HookRegistration { return nil }
func (m *endpointTestModule) Close() error                   { return nil }
func (m *endpointTestModule) Addr() net.Addr                 { return m.addr }

func boolPtr(b bool) *bool { return &b }

func newTestConfig() *config.Config {
	noTLS := boolPtr(false)
	return &config.Config{
		API: config.APIConfig{
			Enabled: true,
			Listen:  ":0",
			TLS:     noTLS,
		},
		Stream: config.StreamConfig{
			GOPCache:           true,
			GOPCacheNum:        1,
			GOPCacheMaxFrames:  config.DefaultGOPCacheMaxFrames,
			GOPCacheMaxBytes:   32 * 1024 * 1024,
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

// decodeAPIData unmarshals the apiResponse envelope and returns the Data portion as raw JSON.
func decodeAPIData(t *testing.T, body []byte) json.RawMessage {
	t.Helper()
	var envelope struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if envelope.Code != 0 {
		t.Fatalf("expected code 0, got %d: %s", envelope.Code, envelope.Message)
	}
	return envelope.Data
}

func newTestHandlers(t *testing.T) (*Handlers, *core.Server) {
	t.Helper()
	cfg := newTestConfig()
	s := core.NewServer(cfg)
	h := NewHandlers(s)
	return h, s
}

func TestHandleStreams_Empty(t *testing.T) {
	h, _ := newTestHandlers(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/streams", nil)
	w := httptest.NewRecorder()
	h.handleStreams(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	data := decodeAPIData(t, w.Body.Bytes())
	var resp StreamsResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Streams) != 0 {
		t.Errorf("expected 0 streams, got %d", len(resp.Streams))
	}
}

func TestHandleStreams_Publishing(t *testing.T) {
	h, s := newTestHandlers(t)

	hub := s.StreamHub()
	stream, err := hub.GetOrCreate("live/test")
	if err != nil {
		t.Fatal(err)
	}
	pub := &testPublisher{
		id: "rtmp-pub-live/test",
		info: &avframe.MediaInfo{
			VideoCodec: avframe.CodecH264,
			AudioCodec: avframe.CodecAAC,
		},
	}
	if err := stream.SetPublisher(pub); err != nil {
		t.Fatal(err)
	}

	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe, 0, 0, []byte{0x02},
	))
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe, 20, 20, []byte{0x03},
	))
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 0, 0, []byte{0x00},
	))
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe, 33, 33, []byte{0x01},
	))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/streams", nil)
	w := httptest.NewRecorder()
	h.handleStreams(w, req)

	data := decodeAPIData(t, w.Body.Bytes())
	var resp struct {
		Streams []StreamInfo `json:"streams"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"audio_cache_frames"`) || strings.Contains(string(data), `"audio_cache_duration_ms"`) {
		t.Fatalf("stream response retained removed audio-cache fields: %s", data)
	}
	if len(resp.Streams) != 1 {
		t.Fatalf("expected 1 stream, got %d", len(resp.Streams))
	}

	si := resp.Streams[0]
	if si.Key != "live/test" {
		t.Errorf("expected key live/test, got %s", si.Key)
	}
	if si.State != "publishing" {
		t.Errorf("expected state publishing, got %s", si.State)
	}
	if si.Publisher != "rtmp-pub-live/test" {
		t.Errorf("expected publisher rtmp-pub-live/test, got %s", si.Publisher)
	}
	if si.VideoCodec != "H264" {
		t.Errorf("expected video codec H264, got %s", si.VideoCodec)
	}
	if si.AudioCodec != "AAC" {
		t.Errorf("expected audio codec AAC, got %s", si.AudioCodec)
	}
	if si.GOPCacheLen != 2 {
		t.Errorf("expected gop_cache_len 2, got %d", si.GOPCacheLen)
	}
	if si.GOPGeneration != 1 {
		t.Errorf("expected gop_generation 1, got %d", si.GOPGeneration)
	}
	if si.Stats == nil {
		t.Error("expected stats in stream list response")
	}
}

func TestBuildStreamInfoReportsTranscodeTasks(t *testing.T) {
	_, s := newTestHandlers(t)
	stream, err := s.StreamHub().GetOrCreate("live/transcode")
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.SetPublisher(&testPublisher{id: "aac-pub", info: &avframe.MediaInfo{AudioCodec: avframe.CodecAAC}}); err != nil {
		t.Fatal(err)
	}
	tm := core.NewTranscodeManager(stream, &audiocodec.Registry{}, 16)
	core.SetTranscodeManagerForTest(stream, tm)
	reader, release, err := tm.GetOrCreateReader(avframe.CodecOpus)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	defer reader.Close()

	info := buildStreamInfo(stream, false)
	if len(info.TranscodeTasks) != 1 {
		t.Fatalf("transcode task count = %d, want 1", len(info.TranscodeTasks))
	}
	task := info.TranscodeTasks[0]
	if task.SourceCodec != "AAC" || task.TargetCodec != "Opus" || task.State != "running" || task.Subscribers != 1 {
		t.Fatalf("transcode task = %+v", task)
	}
}

func TestHandleStreams_Subscribers(t *testing.T) {
	h, s := newTestHandlers(t)

	hub := s.StreamHub()
	stream, err := hub.GetOrCreate("live/test")
	if err != nil {
		t.Fatal(err)
	}
	pub := &testPublisher{
		id:   "rtmp-pub-live/test",
		info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264},
	}
	if err := stream.SetPublisher(pub); err != nil {
		t.Fatal(err)
	}

	mm := stream.MuxerManager()
	mm.GetOrCreateMuxer("flv")
	mm.GetOrCreateMuxer("flv")
	mm.GetOrCreateMuxer("ts")

	stream.AddSubscriber("rtmp")
	stream.AddSubscriber("rtmp")
	stream.AddSubscriber("rtmp")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/streams", nil)
	w := httptest.NewRecorder()
	h.handleStreams(w, req)

	data := decodeAPIData(t, w.Body.Bytes())
	var resp StreamsResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Streams) != 1 {
		t.Fatalf("expected 1 stream, got %d", len(resp.Streams))
	}

	subs := resp.Streams[0].Subscribers
	if subs["flv"] != 2 {
		t.Errorf("expected 2 flv subscribers, got %d", subs["flv"])
	}
	if subs["ts"] != 1 {
		t.Errorf("expected 1 ts subscriber, got %d", subs["ts"])
	}
	if subs["rtmp"] != 3 {
		t.Errorf("expected 3 rtmp subscribers, got %d", subs["rtmp"])
	}
}

func TestHandleStreamDetail(t *testing.T) {
	h, s := newTestHandlers(t)

	hub := s.StreamHub()
	stream, err := hub.GetOrCreate("live/detail")
	if err != nil {
		t.Fatal(err)
	}
	pub := &testPublisher{id: "pub-1", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}
	if err := stream.SetPublisher(pub); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/streams/live/detail", nil)
	w := httptest.NewRecorder()
	h.handleStreamDetail(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	data := decodeAPIData(t, w.Body.Bytes())
	var si StreamInfo
	if err := json.Unmarshal(data, &si); err != nil {
		t.Fatal(err)
	}
	if si.Key != "live/detail" {
		t.Errorf("expected key live/detail, got %s", si.Key)
	}
	if si.Stats == nil {
		t.Error("expected stats in detail response")
	}
}

func TestHandleStreamDelete(t *testing.T) {
	h, s := newTestHandlers(t)

	hub := s.StreamHub()
	if _, err := hub.GetOrCreate("live/del"); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/streams/live/del", nil)
	w := httptest.NewRecorder()
	h.handleStreamDelete(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	if hub.Count() != 0 {
		t.Errorf("expected 0 streams after delete, got %d", hub.Count())
	}
}

func TestHandleKick(t *testing.T) {
	h, s := newTestHandlers(t)
	var stoppedPublisherID string
	s.GetEventBus().Register(core.HookRegistration{
		Event: core.EventPublishStop,
		Mode:  core.HookSync,
		Handler: func(ctx *core.EventContext) error {
			stoppedPublisherID = ctx.PublisherID
			return nil
		},
	})

	hub := s.StreamHub()
	stream, err := hub.GetOrCreate("live/kick")
	if err != nil {
		t.Fatal(err)
	}
	pub := &testPublisher{id: "pub-kick", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}
	if err := stream.SetPublisher(pub); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/streams/live/kick/kick", nil)
	w := httptest.NewRecorder()
	h.handleKick(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if stream.Publisher() != nil {
		t.Error("expected publisher to be removed after kick")
	}
	if stoppedPublisherID != pub.ID() {
		t.Fatalf("stop publisher ID = %q, want %q", stoppedPublisherID, pub.ID())
	}
}

func TestHandleHealth(t *testing.T) {
	h, _ := newTestHandlers(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/server/health", nil)
	w := httptest.NewRecorder()
	h.handleHealth(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestHandleServerInfo(t *testing.T) {
	h, s := newTestHandlers(t)
	s.Config().DVR.Enabled = true
	s.Config().DVR.Listen = "127.0.0.1:8070"

	req := httptest.NewRequest(http.MethodGet, "/api/v1/server/info", nil)
	w := httptest.NewRecorder()
	h.handleServerInfo(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	data := decodeAPIData(t, w.Body.Bytes())
	var info ServerInfo
	if err := json.Unmarshal(data, &info); err != nil {
		t.Fatal(err)
	}
	if info.Version == "" {
		t.Error("expected non-empty version")
	}
	if info.Capabilities.AudioTranscoding {
		t.Error("audio transcoding capability must be false when it is disabled")
	}
	if got := info.Endpoints["dvr"]; got != "127.0.0.1:8070" {
		t.Errorf("dvr endpoint=%q", got)
	}
}

func TestHandleServerInfoUsesBoundEndpoint(t *testing.T) {
	h, s := newTestHandlers(t)
	bound := &endpointTestModule{name: "httpstream", addr: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 18080}}
	s.RegisterModule(bound)
	s.Config().HTTP.Enabled = true
	s.Config().HTTP.Listen = ":0"

	req := httptest.NewRequest(http.MethodGet, "/api/v1/server/info", nil)
	w := httptest.NewRecorder()
	h.handleServerInfo(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	data := decodeAPIData(t, w.Body.Bytes())
	var info ServerInfo
	if err := json.Unmarshal(data, &info); err != nil {
		t.Fatal(err)
	}
	if got := info.Endpoints["http"]; got != "127.0.0.1:18080" {
		t.Fatalf("http endpoint = %q, want bound endpoint", got)
	}
}

func TestHandleServerInfoUsesBoundDVRListener(t *testing.T) {
	cfg := config.Defaults()
	cfg.DVR.Enabled = true
	cfg.DVR.Listen = "127.0.0.1:0"
	cfg.DVR.Path = t.TempDir() + "/{stream_key}"
	server := core.NewServer(cfg)
	dvrModule := dvr.NewModule()
	server.RegisterModule(dvrModule)
	if err := server.Init(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Shutdown)

	h := NewHandlers(server)
	response := httptest.NewRecorder()
	h.handleServerInfo(response, httptest.NewRequest(http.MethodGet, "/api/v1/server/info", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	var info struct {
		Endpoints map[string]string `json:"endpoints"`
	}
	if err := json.Unmarshal(decodeAPIData(t, response.Body.Bytes()), &info); err != nil {
		t.Fatal(err)
	}
	endpoint := info.Endpoints["dvr"]
	if endpoint == "" || strings.HasSuffix(endpoint, ":0") {
		t.Fatalf("dvr endpoint = %q, want the bound non-zero listener port", endpoint)
	}
	if _, port, err := net.SplitHostPort(endpoint); err != nil || port == "0" {
		t.Fatalf("dvr endpoint = %q, want a host:port with a non-zero port", endpoint)
	}
}

func TestHandleServerInfoReportsDVRTLScheme(t *testing.T) {
	cfg := config.Defaults()
	cfg.TLS.Auto = true
	cfg.DVR.Enabled = true
	cfg.DVR.Listen = "127.0.0.1:0"
	cfg.DVR.Path = t.TempDir() + "/{stream_key}"
	server := core.NewServer(cfg)
	server.RegisterModule(dvr.NewModule())
	if err := server.Init(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Shutdown)

	h := NewHandlers(server)
	response := httptest.NewRecorder()
	h.handleServerInfo(response, httptest.NewRequest(http.MethodGet, "/api/v1/server/info", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	var info struct {
		EndpointSchemes map[string]string `json:"endpoint_schemes"`
	}
	if err := json.Unmarshal(decodeAPIData(t, response.Body.Bytes()), &info); err != nil {
		t.Fatal(err)
	}
	if got := info.EndpointSchemes["dvr"]; got != "https" {
		t.Fatalf("dvr endpoint scheme = %q, want https", got)
	}
}

func TestHandleServerStats(t *testing.T) {
	h, _ := newTestHandlers(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/server/stats", nil)
	w := httptest.NewRecorder()
	h.handleServerStats(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	data := decodeAPIData(t, w.Body.Bytes())
	var stats ServerStats
	if err := json.Unmarshal(data, &stats); err != nil {
		t.Fatal(err)
	}
}

func TestHandleConfigStatus(t *testing.T) {
	h, server := newTestHandlers(t)
	source := &testConfigSource{}
	manager, err := configruntime.NewManager(configruntime.Options{Source: source, Initial: config.Defaults()})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	server.SetConfigManager(manager)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/server/config", nil)
	w := httptest.NewRecorder()
	h.handleConfigStatus(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	data := decodeAPIData(t, w.Body.Bytes())
	var status map[string]any
	if err := json.Unmarshal(data, &status); err != nil {
		t.Fatal(err)
	}
	if status["source"] != "custom" {
		t.Fatalf("status = %v", status)
	}
	for _, field := range []string{"config_changes_accepted", "config_changes_rejected", "config_changes_application_failed"} {
		if _, ok := status[field]; !ok {
			t.Fatalf("config status omitted %q: %v", field, status)
		}
	}
}

func TestHandleConfigStatusRedactsBackgroundSourceError(t *testing.T) {
	h, server := newTestHandlers(t)
	const sourceURL = "https://status-user:status-password@config.example.test/live.yaml?token=query-secret" //nolint:gosec // Synthetic value verifies redaction.
	manager, err := configruntime.NewManager(configruntime.Options{
		Source:       errorConfigWriterSource{err: errors.New("refresh " + sourceURL + "\nretry denied")},
		Initial:      config.Defaults(),
		PollInterval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for manager.Status().ConsecutiveFailures == 0 {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for background source failure")
		}
		time.Sleep(time.Millisecond)
	}
	server.SetConfigManager(manager)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/server/config", nil)
	w := httptest.NewRecorder()
	h.handleConfigStatus(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	data := decodeAPIData(t, w.Body.Bytes())
	var status ConfigRuntimeStatus
	if err := json.Unmarshal(data, &status); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"status-user", "status-password", "query-secret", "token="} {
		if strings.Contains(status.LastError, secret) {
			t.Fatalf("config status leaked %q: %q", secret, status.LastError)
		}
	}
	if !strings.Contains(status.LastError, "config.example.test") || strings.ContainsAny(status.LastError, "\r\n") {
		t.Fatalf("config status lost context or retained line breaks: %q", status.LastError)
	}
}

func TestHandleConfigDocumentReturnsRedactedEffectiveConfigAndSchema(t *testing.T) {
	h, server := newTestHandlers(t)
	cfg := config.Defaults()
	cfg.API.Auth.BearerToken = "api-secret"
	cfg.API.Console.Username = "admin"
	cfg.API.Console.Password = "console-secret"
	server.UpdateConfig(cfg)
	manager, err := configruntime.NewManager(configruntime.Options{Source: &testConfigSource{}, Initial: cfg})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	server.SetConfigManager(manager)

	w := httptest.NewRecorder()
	h.handleConfigDocument(w, httptest.NewRequest(http.MethodGet, "/api/v1/server/config/document", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	data := decodeAPIData(t, w.Body.Bytes())
	var response struct {
		Effective map[string]any `json:"effective"`
		Schema    map[string]any `json:"schema"`
		Writable  bool           `json:"writable"`
		Source    map[string]any `json:"source_details"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		t.Fatal(err)
	}
	if response.Writable || response.Schema["$schema"] == nil || response.Schema["$defs"] == nil || response.Source["kind"] == nil {
		t.Fatalf("document response missing metadata: %+v", response)
	}
	apiConfig, ok := response.Effective["api"].(map[string]any)
	if !ok {
		t.Fatalf("effective config missing api section: %+v", response.Effective)
	}
	auth, _ := apiConfig["auth"].(map[string]any)
	if auth["bearer_token"] != "[REDACTED]" {
		t.Fatalf("bearer token was not redacted: %+v", auth)
	}
	console, _ := apiConfig["console"].(map[string]any)
	if console["password"] != "[REDACTED]" {
		t.Fatalf("console password was not redacted: %+v", console)
	}
}

func TestHandleConfigValidateRejectsInvalidDocument(t *testing.T) {
	h, _ := newTestHandlers(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/server/config/validate", strings.NewReader(`{"document":"server:\n  name: ["}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.handleConfigValidate(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

type testConfigSource struct{}

func (testConfigSource) Load(context.Context, configruntime.Version) (configruntime.Snapshot, error) {
	return configruntime.Snapshot{}, nil
}

func (testConfigSource) Close() error { return nil }
