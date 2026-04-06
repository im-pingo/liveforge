package httpstream

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

func TestParseStreamPath(t *testing.T) {
	tests := []struct {
		path   string
		app    string
		key    string
		format string
		ok     bool
	}{
		{"/live/test.flv", "live", "test", "flv", true},
		{"/live/test.ts", "live", "test", "ts", true},
		{"/app/stream.mp4", "app", "stream", "mp4", true},
		{"/live/multi/part.flv", "live", "multi/part", "flv", true},
		{"/noext", "", "", "", false},
		{"/", "", "", "", false},
		{"/.flv", "", "", "", false},
	}
	for _, tt := range tests {
		app, key, format, ok := parseStreamPath(tt.path)
		if ok != tt.ok || app != tt.app || key != tt.key || format != tt.format {
			t.Errorf("parseStreamPath(%q) = (%q,%q,%q,%v), want (%q,%q,%q,%v)",
				tt.path, app, key, format, ok, tt.app, tt.key, tt.format, tt.ok)
		}
	}
}

func TestParseSegmentPath(t *testing.T) {
	tests := []struct {
		path    string
		app     string
		key     string
		segName string
		ext     string
		ok      bool
	}{
		{"/live/test/0.ts", "live", "test", "0", "ts", true},
		{"/live/test/init.mp4", "live", "test", "init", "mp4", true},
		{"/live/test/v1.m4s", "live", "test", "v1", "m4s", true},
		{"/live/test/a1.m4s", "live", "test", "a1", "m4s", true},
		{"/live/test/vinit.mp4", "live", "test", "vinit", "mp4", true},
		{"/live/test/audio_init.mp4", "live", "test", "audio_init", "mp4", true},
		{"/notenough", "", "", "", "", false},
		{"/a/b", "", "", "", "", false},
		{"/a/b/nodot", "", "", "", "", false},
	}
	for _, tt := range tests {
		app, key, segName, ext, ok := parseSegmentPath(tt.path)
		if ok != tt.ok || app != tt.app || key != tt.key || segName != tt.segName || ext != tt.ext {
			t.Errorf("parseSegmentPath(%q) = (%q,%q,%q,%q,%v), want (%q,%q,%q,%q,%v)",
				tt.path, app, key, segName, ext, ok, tt.app, tt.key, tt.segName, tt.ext, tt.ok)
		}
	}
}

// newHTTPTestServer creates a Module wired to a core.Server for HTTP handler tests.
func newHTTPTestServer(t *testing.T) (*Module, *core.Server, string) {
	t.Helper()

	noTLS := false
	cfg := &config.Config{}
	cfg.HTTP.Listen = "127.0.0.1:0"
	cfg.HTTP.TLS = &noTLS
	cfg.HTTP.CORS = true
	cfg.Stream.RingBufferSize = 256

	srv := core.NewServer(cfg)
	m := NewModule()
	srv.RegisterModule(m)
	if err := srv.Init(); err != nil {
		t.Fatalf("server init: %v", err)
	}
	t.Cleanup(srv.Shutdown)

	addr := "http://" + m.Addr().String()
	return m, srv, addr
}

func TestHandlerMethodNotAllowed(t *testing.T) {
	_, _, addr := newHTTPTestServer(t)

	resp, err := http.Post(addr+"/live/test.flv", "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", resp.StatusCode)
	}
}

func TestHandlerOptionsRequest(t *testing.T) {
	_, _, addr := newHTTPTestServer(t)

	req, _ := http.NewRequest("OPTIONS", addr+"/live/test.flv", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	if cors := resp.Header.Get("Access-Control-Allow-Origin"); cors != "*" {
		t.Errorf("expected CORS header '*', got %q", cors)
	}
}

func TestHandlerInvalidPath(t *testing.T) {
	_, _, addr := newHTTPTestServer(t)

	resp, err := http.Get(addr + "/badpath")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

func TestHandlerUnsupportedFormat(t *testing.T) {
	_, _, addr := newHTTPTestServer(t)

	resp, err := http.Get(addr + "/live/test.mkv")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

func TestHandlerStreamNotFound(t *testing.T) {
	_, _, addr := newHTTPTestServer(t)

	resp, err := http.Get(addr + "/live/nonexist.flv")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestHandlerFLVStream(t *testing.T) {
	m, srv, addr := newHTTPTestServer(t)

	stream, err := srv.StreamHub().GetOrCreate("live/flvtest")
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.SetPublisher(dummyPublisher{}); err != nil {
		t.Fatal(err)
	}

	// Register test muxer callback
	m.registeredMu.Lock()
	m.registered[stream] = true
	m.registeredMu.Unlock()

	mm := stream.MuxerManager()
	mm.RegisterMuxerStart("flv", func(inst *core.MuxerInstance, s *core.Stream) {
		go func() {
			defer inst.Buffer.Close()
			inst.SetInitData([]byte("FLV-HEADER"))
			inst.Buffer.Write([]byte("flv-data-1"))
			inst.Buffer.Write([]byte("flv-data-2"))
		}()
	})

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(addr + "/live/flvtest.flv")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "video/x-flv" {
		t.Errorf("expected Content-Type video/x-flv, got %q", ct)
	}

	body, _ := io.ReadAll(resp.Body)
	content := string(body)
	if !strings.Contains(content, "FLV-HEADER") {
		t.Error("response should contain FLV header")
	}
	if !strings.Contains(content, "flv-data-1") {
		t.Error("response should contain flv-data-1")
	}
}

func TestHandlerTSStream(t *testing.T) {
	m, srv, addr := newHTTPTestServer(t)

	stream, err := srv.StreamHub().GetOrCreate("live/tstest")
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.SetPublisher(dummyPublisher{}); err != nil {
		t.Fatal(err)
	}

	m.registeredMu.Lock()
	m.registered[stream] = true
	m.registeredMu.Unlock()

	mm := stream.MuxerManager()
	mm.RegisterMuxerStart("ts", func(inst *core.MuxerInstance, s *core.Stream) {
		go func() {
			defer inst.Buffer.Close()
			inst.Buffer.Write([]byte("ts-packet-1"))
			inst.Buffer.Write([]byte("ts-packet-2"))
		}()
	})

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(addr + "/live/tstest.ts")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "video/mp2t" {
		t.Errorf("expected Content-Type video/mp2t, got %q", ct)
	}
}

func TestHandlerFMP4Stream(t *testing.T) {
	m, srv, addr := newHTTPTestServer(t)

	stream, err := srv.StreamHub().GetOrCreate("live/fmp4test")
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.SetPublisher(dummyPublisher{}); err != nil {
		t.Fatal(err)
	}

	m.registeredMu.Lock()
	m.registered[stream] = true
	m.registeredMu.Unlock()

	mm := stream.MuxerManager()
	mm.RegisterMuxerStart("mp4", func(inst *core.MuxerInstance, s *core.Stream) {
		go func() {
			defer inst.Buffer.Close()
			inst.SetInitData([]byte("INIT-SEG"))
			inst.Buffer.Write([]byte("fmp4-data"))
		}()
	})

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(addr + "/live/fmp4test.mp4")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "video/mp4" {
		t.Errorf("expected Content-Type video/mp4, got %q", ct)
	}
}

func TestHandlerMaxConnections(t *testing.T) {
	noTLS := false
	cfg := &config.Config{}
	cfg.HTTP.Listen = "127.0.0.1:0"
	cfg.HTTP.TLS = &noTLS
	cfg.HTTP.CORS = true
	cfg.Stream.RingBufferSize = 256
	cfg.Limits.MaxConnections = 1

	srv := core.NewServer(cfg)
	m := NewModule()
	srv.RegisterModule(m)
	if err := srv.Init(); err != nil {
		t.Fatalf("server init: %v", err)
	}
	t.Cleanup(srv.Shutdown)

	addr := "http://" + m.Addr().String()

	// Create a publishing stream with a blocking muxer
	stream, err := srv.StreamHub().GetOrCreate("live/conn")
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.SetPublisher(dummyPublisher{}); err != nil {
		t.Fatal(err)
	}

	m.registeredMu.Lock()
	m.registered[stream] = true
	m.registeredMu.Unlock()

	blockCh := make(chan struct{})
	mm := stream.MuxerManager()
	mm.RegisterMuxerStart("ts", func(inst *core.MuxerInstance, s *core.Stream) {
		go func() {
			defer inst.Buffer.Close()
			inst.Buffer.Write([]byte("data"))
			<-blockCh
		}()
	})

	// First connection should succeed
	client := &http.Client{Timeout: 2 * time.Second}
	resp1, err := client.Get(addr + "/live/conn.ts")
	if err != nil {
		t.Fatal(err)
	}

	// Read some data to ensure connection is established
	buf := make([]byte, 4)
	resp1.Body.Read(buf)

	// Second connection should get 503
	resp2, err := http.Get(addr + "/live/conn.ts")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", resp2.StatusCode)
	}

	// Cleanup
	close(blockCh)
	resp1.Body.Close()
}

func TestHandlerHLSPlaylistNotFound(t *testing.T) {
	_, _, addr := newHTTPTestServer(t)

	resp, err := http.Get(addr + "/live/test.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestHandlerDASHManifestNotFound(t *testing.T) {
	_, _, addr := newHTTPTestServer(t)

	resp, err := http.Get(addr + "/live/test.mpd")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestHandlerHLSSegmentNotFound(t *testing.T) {
	_, _, addr := newHTTPTestServer(t)

	resp, err := http.Get(addr + "/live/test/0.ts")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// The stream doesn't exist, so we get either 404 or 400
	if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 404 or 400, got %d", resp.StatusCode)
	}
}

func TestHandlerDASHSegmentInvalid(t *testing.T) {
	_, _, addr := newHTTPTestServer(t)

	resp, err := http.Get(addr + "/live/test/xinvalid.m4s")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

func TestHandlerDASHVideoSegmentNotFound(t *testing.T) {
	_, _, addr := newHTTPTestServer(t)

	resp, err := http.Get(addr + "/live/test/v1.m4s")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestHandlerDASHAudioSegmentNotFound(t *testing.T) {
	_, _, addr := newHTTPTestServer(t)

	resp, err := http.Get(addr + "/live/test/a1.m4s")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestHandlerDASHVideoInitNotFound(t *testing.T) {
	_, _, addr := newHTTPTestServer(t)

	resp, err := http.Get(addr + "/live/test/vinit.mp4")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestHandlerDASHAudioInitNotFound(t *testing.T) {
	_, _, addr := newHTTPTestServer(t)

	resp, err := http.Get(addr + "/live/test/audio_init.mp4")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestHandlerDASHInitNotFound(t *testing.T) {
	_, _, addr := newHTTPTestServer(t)

	resp, err := http.Get(addr + "/live/test/init.mp4")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestHandlerHLSPlaylistWithStream(t *testing.T) {
	m, srv, addr := newHTTPTestServer(t)

	stream, err := srv.StreamHub().GetOrCreate("live/hlstest")
	if err != nil {
		t.Fatal(err)
	}
	pub := &mediaPublisher{
		info: &avframe.MediaInfo{
			VideoCodec: avframe.CodecH264,
			AudioCodec: avframe.CodecAAC,
		},
	}
	if err := stream.SetPublisher(pub); err != nil {
		t.Fatal(err)
	}

	// Pre-create HLS manager and add a segment so the playlist is immediately available
	hlsMgr := m.getOrCreateHLS("live/hlstest", stream)
	hlsMgr.mu.Lock()
	hlsMgr.segments = append(hlsMgr.segments, &HLSSegment{SeqNum: 0, Duration: 2.0, Data: []byte("seg-data")})
	hlsMgr.nextSeqNum = 1
	hlsMgr.mu.Unlock()

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(addr + "/live/hlstest.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if ct != "application/vnd.apple.mpegurl" {
		t.Errorf("expected m3u8 content type, got %q", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "#EXTM3U") {
		t.Error("playlist should contain #EXTM3U")
	}
}

func TestHandlerDASHManifestWithStream(t *testing.T) {
	m, srv, addr := newHTTPTestServer(t)

	stream, err := srv.StreamHub().GetOrCreate("live/dashtest")
	if err != nil {
		t.Fatal(err)
	}
	pub := &mediaPublisher{
		info: &avframe.MediaInfo{
			VideoCodec: avframe.CodecH264,
			AudioCodec: avframe.CodecAAC,
		},
	}
	if err := stream.SetPublisher(pub); err != nil {
		t.Fatal(err)
	}

	// Pre-create DASH manager and inject segments so the handler doesn't block
	dashMgr := m.getOrCreateDASH("live/dashtest", stream)
	dashMgr.mu.Lock()
	for i := range 3 {
		dashMgr.videoSegments = append(dashMgr.videoSegments, &DASHSegment{SeqNum: i, Duration: 2.0, Data: []byte("seg")})
	}
	dashMgr.nextSeqNum = 3
	dashMgr.videoCodecStr = "avc1.640028"
	dashMgr.mu.Unlock()

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(addr + "/live/dashtest.mpd")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// DASH manifest should be available (may be empty but 200)
	if resp.StatusCode == http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		content := string(body)
		if !strings.Contains(content, "MPD") {
			t.Error("manifest should contain MPD")
		}
	}
}

func TestQueryToMap(t *testing.T) {
	tests := []struct {
		name string
		vals map[string][]string
		want map[string]string
	}{
		{"nil", nil, nil},
		{"empty", map[string][]string{}, nil},
		{"single", map[string][]string{"key": {"val"}}, map[string]string{"key": "val"}},
		{"multi", map[string][]string{"k": {"v1", "v2"}}, map[string]string{"k": "v1"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := queryToMap(tt.vals)
			if tt.want == nil {
				if got != nil {
					t.Errorf("expected nil, got %v", got)
				}
				return
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("key %q: got %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

func TestHandlerHLSSegmentServing(t *testing.T) {
	m, srv, addr := newHTTPTestServer(t)

	stream, err := srv.StreamHub().GetOrCreate("live/hlsseg")
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.SetPublisher(dummyPublisher{}); err != nil {
		t.Fatal(err)
	}

	// Pre-create HLS manager with a segment
	hlsMgr := m.getOrCreateHLS("live/hlsseg", stream)
	hlsMgr.mu.Lock()
	hlsMgr.segments = append(hlsMgr.segments, &HLSSegment{SeqNum: 0, Duration: 2.0, Data: []byte("ts-segment-0")})
	hlsMgr.nextSeqNum = 1
	hlsMgr.mu.Unlock()

	client := &http.Client{Timeout: 2 * time.Second}

	// Request existing segment
	resp, err := client.Get(addr + "/live/hlsseg/0.ts")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ts-segment-0" {
		t.Errorf("expected segment data, got %q", body)
	}

	// Request non-existing segment
	resp2, err := client.Get(addr + "/live/hlsseg/99.ts")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp2.StatusCode)
	}
}

func TestHandlerDASHInitAndSegmentServing(t *testing.T) {
	m, srv, addr := newHTTPTestServer(t)

	stream, err := srv.StreamHub().GetOrCreate("live/dashseg")
	if err != nil {
		t.Fatal(err)
	}
	pub := &mediaPublisher{
		info: &avframe.MediaInfo{
			VideoCodec: avframe.CodecH264,
			AudioCodec: avframe.CodecAAC,
		},
	}
	if err := stream.SetPublisher(pub); err != nil {
		t.Fatal(err)
	}

	// Pre-create DASH manager with init segments and media segments
	dashMgr := m.getOrCreateDASH("live/dashseg", stream)
	dashMgr.mu.Lock()
	dashMgr.videoInitSeg = []byte("video-init")
	dashMgr.audioInitSeg = []byte("audio-init")
	dashMgr.videoSegments = append(dashMgr.videoSegments, &DASHSegment{SeqNum: 0, Duration: 2.0, Data: []byte("video-seg-0")})
	dashMgr.audioSegments = append(dashMgr.audioSegments, &DASHSegment{SeqNum: 0, Duration: 2.0, Data: []byte("audio-seg-0")})
	dashMgr.nextSeqNum = 1
	dashMgr.hasAudio = true
	dashMgr.mu.Unlock()

	client := &http.Client{Timeout: 2 * time.Second}

	// Video init segment
	resp, err := client.Get(addr + "/live/dashseg/vinit.mp4")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("vinit: expected 200, got %d", resp.StatusCode)
	}
	if string(body) != "video-init" {
		t.Errorf("vinit: got %q", body)
	}

	// Audio init segment
	resp, err = client.Get(addr + "/live/dashseg/audio_init.mp4")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("audio_init: expected 200, got %d", resp.StatusCode)
	}
	if string(body) != "audio-init" {
		t.Errorf("audio_init: got %q", body)
	}

	// Video segment (1-based in URL, 0-based internal)
	resp, err = client.Get(addr + "/live/dashseg/v1.m4s")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("v1.m4s: expected 200, got %d", resp.StatusCode)
	}
	if string(body) != "video-seg-0" {
		t.Errorf("v1.m4s: got %q", body)
	}

	// Audio segment
	resp, err = client.Get(addr + "/live/dashseg/a1.m4s")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("a1.m4s: expected 200, got %d", resp.StatusCode)
	}
	if string(body) != "audio-seg-0" {
		t.Errorf("a1.m4s: got %q", body)
	}

	// init.mp4 falls back to DASH video init when no LL-HLS manager
	resp, err = client.Get(addr + "/live/dashseg/init.mp4")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("init.mp4: expected 200, got %d", resp.StatusCode)
	}
	if string(body) != "video-init" {
		t.Errorf("init.mp4: got %q", body)
	}
}

func TestModuleName(t *testing.T) {
	m := NewModule()
	if m.Name() != "httpstream" {
		t.Errorf("expected 'httpstream', got %q", m.Name())
	}
}

func TestCleanupManagers(t *testing.T) {
	m, srv, _ := newHTTPTestServer(t)

	stream, err := srv.StreamHub().GetOrCreate("live/cleanup")
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.SetPublisher(dummyPublisher{}); err != nil {
		t.Fatal(err)
	}

	// Create managers
	_ = m.getOrCreateHLS("live/cleanup", stream)
	_ = m.getOrCreateDASH("live/cleanup", stream)

	// Verify they exist
	m.hlsMu.Lock()
	_, hlsExists := m.hlsManagers["live/cleanup"]
	m.hlsMu.Unlock()
	m.dashMu.Lock()
	_, dashExists := m.dashManagers["live/cleanup"]
	m.dashMu.Unlock()

	if !hlsExists || !dashExists {
		t.Fatal("managers should exist before cleanup")
	}

	// Trigger cleanup
	m.cleanupManagers("live/cleanup")

	// Verify they're gone
	m.hlsMu.Lock()
	_, hlsExists = m.hlsManagers["live/cleanup"]
	m.hlsMu.Unlock()
	m.dashMu.Lock()
	_, dashExists = m.dashManagers["live/cleanup"]
	m.dashMu.Unlock()

	if hlsExists || dashExists {
		t.Error("managers should be cleaned up")
	}
}

func TestHandlerCORSHeaders(t *testing.T) {
	_, _, addr := newHTTPTestServer(t)

	req, _ := http.NewRequest("OPTIONS", addr+"/live/test.flv", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if v := resp.Header.Get("Access-Control-Allow-Origin"); v != "*" {
		t.Errorf("CORS origin: got %q, want *", v)
	}
	if v := resp.Header.Get("Access-Control-Allow-Methods"); v == "" {
		t.Error("CORS methods header missing")
	}
}

// mediaPublisher is a test publisher that provides MediaInfo.
type mediaPublisher struct {
	info *avframe.MediaInfo
}

func (p *mediaPublisher) ID() string                   { return "test-media-pub" }
func (p *mediaPublisher) MediaInfo() *avframe.MediaInfo { return p.info }
func (p *mediaPublisher) Close() error                  { return nil }
