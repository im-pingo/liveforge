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

func newTestConfig() *config.Config {
	return &config.Config{
		API: config.APIConfig{
			Enabled: true,
			Listen:  ":0",
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

func TestHandleStreams_Empty(t *testing.T) {
	cfg := newTestConfig()
	s := core.NewServer(cfg)

	m := NewModule()
	if err := m.Init(s); err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/streams", nil)
	w := httptest.NewRecorder()
	m.handleStreams(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp StreamsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Streams) != 0 {
		t.Errorf("expected 0 streams, got %d", len(resp.Streams))
	}
}

func TestHandleStreams_Publishing(t *testing.T) {
	cfg := newTestConfig()
	s := core.NewServer(cfg)

	hub := s.StreamHub()
	stream := hub.GetOrCreate("live/test")
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

	// Write a keyframe + inter-frame to populate GOP cache
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 0, 0, []byte{0x00},
	))
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe, 33, 33, []byte{0x01},
	))

	m := NewModule()
	if err := m.Init(s); err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/streams", nil)
	w := httptest.NewRecorder()
	m.handleStreams(w, req)

	var resp StreamsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
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
}

func TestHandleStreams_Subscribers(t *testing.T) {
	cfg := newTestConfig()
	s := core.NewServer(cfg)

	hub := s.StreamHub()
	stream := hub.GetOrCreate("live/test")
	pub := &testPublisher{
		id:   "rtmp-pub-live/test",
		info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264},
	}
	if err := stream.SetPublisher(pub); err != nil {
		t.Fatal(err)
	}

	// Add HTTP subscribers via MuxerManager
	mm := stream.MuxerManager()
	mm.GetOrCreateMuxer("flv")
	mm.GetOrCreateMuxer("flv")
	mm.GetOrCreateMuxer("ts")

	// Add protocol-level subscribers
	stream.AddSubscriber("rtmp")
	stream.AddSubscriber("rtmp")
	stream.AddSubscriber("rtmp")

	m := NewModule()
	if err := m.Init(s); err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/streams", nil)
	w := httptest.NewRecorder()
	m.handleStreams(w, req)

	var resp StreamsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
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
