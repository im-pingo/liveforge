package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
)

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
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 0, 0, []byte{0x00},
	))
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe, 33, 33, []byte{0x01},
	))

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
	if si.Stats == nil {
		t.Error("expected stats in stream list response")
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
	h, _ := newTestHandlers(t)

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
