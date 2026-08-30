package httpstream

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
)

type httpTestAuthorizer func(context.Context, core.AuthorizationRequest) error

func (f httpTestAuthorizer) Authorize(ctx context.Context, request core.AuthorizationRequest) error {
	return f(ctx, request)
}

type shortWriteDeadlineListener struct {
	net.Listener
	timeout time.Duration
}

func (l shortWriteDeadlineListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return shortWriteDeadlineConn{Conn: conn, timeout: l.timeout}, nil
}

type shortWriteDeadlineConn struct {
	net.Conn
	timeout time.Duration
}

func (c shortWriteDeadlineConn) SetWriteDeadline(deadline time.Time) error {
	if deadline.IsZero() {
		return c.Conn.SetWriteDeadline(deadline)
	}
	return c.Conn.SetWriteDeadline(time.Now().Add(c.timeout))
}

func newShortWriteDeadlineServer(t *testing.T, handler http.Handler, timeout time.Duration) string {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	server.Listener = shortWriteDeadlineListener{Listener: server.Listener, timeout: timeout}
	server.Start()
	t.Cleanup(server.Close)
	return server.URL
}

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
		{"/s1.flv", "", "s1", "flv", true},
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

func TestModuleCloseTerminatesActiveHTTPSubscriber(t *testing.T) {
	m, srv, addr := newHTTPTestServer(t)
	stream, err := srv.StreamHub().GetOrCreate("live/close-active-http")
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.SetPublisher(dummyPublisher{}); err != nil {
		t.Fatal(err)
	}
	m.registeredMu.Lock()
	m.registered[stream.Key()] = stream.InstanceID()
	m.registeredMu.Unlock()
	stream.MuxerManager().RegisterMuxerStart("ts", func(inst *core.MuxerInstance, _ *core.Stream) {
		inst.Buffer.Write([]byte{0x47, 0x00, 0x00, 0x10})
	})

	resp, err := http.Get(addr + "/live/close-active-http.ts")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	deadline := time.Now().Add(time.Second)
	for srv.ConnectionCount() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := srv.ConnectionCount(); got != 1 {
		t.Fatalf("active connection count = %d, want 1", got)
	}

	closed := make(chan error, 1)
	go func() { closed <- m.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("HTTP module close exceeded the active stream drain bound")
	}
	if got := srv.ConnectionCount(); got != 0 {
		t.Fatalf("connection count after HTTP module close = %d, want 0", got)
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
		{"/s1/0.ts", "", "s1", "0", "ts", true},
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

func TestHTTPContinuousStreamOverwriteDropsRetainedPacket(t *testing.T) {
	for _, format := range []string{"flv", "ts", "mp4"} {
		t.Run(format, func(t *testing.T) {
			buffer := core.NewSharedBuffer(2)
			reader := buffer.NewReader()
			sentinel := []byte("HTTP-CONTINUITY-GAP-SENTINEL-" + format)
			for _, packet := range [][]byte{
				[]byte("old-0"),
				[]byte("old-1"),
				sentinel,
				[]byte("post-gap-tail"),
			} {
				buffer.Write(packet)
			}

			established := []byte("established-" + format)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := writeHTTPStreamChunk(w, established, httpStreamWriteTimeout); err != nil {
					return
				}
				serveHTTPStreamReader(w, r, format, "live/overwrite", reader)
			}))
			t.Cleanup(server.Close)

			response, err := http.Get(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(response.Body)
			response.Body.Close()
			if !bytes.Contains(body, established) {
				t.Fatalf("HTTP %s body = %q, want established bytes %q", format, body, established)
			}
			if bytes.Contains(body, sentinel) {
				t.Fatalf("HTTP %s body contains retained post-gap packet %q", format, sentinel)
			}
		})
	}
}

func TestStreamReturnsServiceUnavailableWhenMuxerInitIsNotReady(t *testing.T) {
	_, server, address := newHTTPTestServer(t)
	stream, err := server.StreamHub().GetOrCreate("live/no-init")
	if err != nil {
		t.Fatal(err)
	}
	if setErr := stream.SetPublisher(&mediaPublisher{info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}); setErr != nil {
		t.Fatal(setErr)
	}

	response, err := http.Get(address + "/live/no-init.flv")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("stream response = %d %q, want 503", response.StatusCode, body)
	}
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

func TestHandlerAuthorizesHLSPlaylistAndSegments(t *testing.T) {
	_, srv, addr := newHTTPTestServer(t)
	var requests []core.AuthorizationRequest
	srv.SetAuthorizer(httpTestAuthorizer(func(_ context.Context, request core.AuthorizationRequest) error {
		requests = append(requests, request)
		return errors.New("denied")
	}))

	for _, path := range []string{"/live/test.m3u8", "/live/test/0.ts"} {
		resp, err := http.Get(addr + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s: status = %d, want %d", path, resp.StatusCode, http.StatusForbidden)
		}
	}
	if len(requests) != 2 {
		t.Fatalf("authorization requests = %d, want 2", len(requests))
	}
	for _, request := range requests {
		if request.Action != core.AuthorizationSubscribe || request.Stage != core.AuthorizationPreSession || request.Protocol != "hls" {
			t.Fatalf("authorization request = %#v", request)
		}
		if request.StreamKey != "live/test" {
			t.Fatalf("authorization stream key = %q", request.StreamKey)
		}
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
	m.registered[stream.Key()] = stream.InstanceID()
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

	unprefixed, err := srv.StreamHub().GetOrCreate("s1")
	if err != nil {
		t.Fatal(err)
	}
	if err := unprefixed.SetPublisher(dummyPublisher{}); err != nil {
		t.Fatal(err)
	}
	m.registeredMu.Lock()
	m.registered[unprefixed.Key()] = unprefixed.InstanceID()
	m.registeredMu.Unlock()
	unprefixed.MuxerManager().RegisterMuxerStart("flv", func(inst *core.MuxerInstance, _ *core.Stream) {
		go func() {
			defer inst.Buffer.Close()
			inst.SetInitData([]byte("UNPREFIXED-FLV-HEADER"))
			inst.Buffer.Write([]byte("unprefixed-flv-data"))
		}()
	})
	resp, err = client.Get(addr + "/s1.flv")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unprefixed FLV status = %d", resp.StatusCode)
	}
	unprefixedBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(unprefixedBody), "unprefixed-flv-data") {
		t.Fatalf("unprefixed FLV body = %q", unprefixedBody)
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
	m.registered[stream.Key()] = stream.InstanceID()
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
	m.registered[stream.Key()] = stream.InstanceID()
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
	m.registered[stream.Key()] = stream.InstanceID()
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

	// A single completed keyframe-bounded segment is sufficient to start DASH.
	dashMgr := m.getOrCreateDASH("live/dashtest", stream)
	dashMgr.mu.Lock()
	dashMgr.videoSegments = append(dashMgr.videoSegments, &DASHSegment{SeqNum: 0, Duration: 8.3, Data: []byte("seg")})
	dashMgr.nextSeqNum = 1
	dashMgr.videoCodecStr = "avc1.640028"
	dashMgr.mu.Unlock()

	type result struct {
		status int
		body   string
		err    error
	}
	resultCh := make(chan result, 1)
	go func() {
		resp, err := (&http.Client{Timeout: 2 * time.Second}).Get(addr + "/live/dashtest.mpd")
		if err != nil {
			resultCh <- result{err: err}
			return
		}
		defer resp.Body.Close()
		body, readErr := io.ReadAll(resp.Body)
		resultCh <- result{status: resp.StatusCode, body: string(body), err: readErr}
	}()

	var got result
	releasedWithOneSegment := true
	select {
	case got = <-resultCh:
	case <-time.After(300 * time.Millisecond):
		releasedWithOneSegment = false
		// Unblock the old three-segment behavior so the regression fails fast.
		dashMgr.mu.Lock()
		dashMgr.videoSegments = append(dashMgr.videoSegments,
			&DASHSegment{SeqNum: 1, Duration: 8.3, Data: []byte("seg")},
			&DASHSegment{SeqNum: 2, Duration: 8.3, Data: []byte("seg")},
		)
		dashMgr.nextSeqNum = 3
		dashMgr.mu.Unlock()
		got = <-resultCh
	}
	if got.err != nil {
		t.Fatal(got.err)
	}
	if !releasedWithOneSegment {
		t.Fatal("DASH manifest waited for more than the first completed segment")
	}
	if got.status != http.StatusOK || !strings.Contains(got.body, "MPD") {
		t.Fatalf("manifest response = status %d body %q", got.status, got.body)
	}
}

func TestHandlerDelayedManifestReadinessRefreshesWriteDeadline(t *testing.T) {
	for _, test := range []struct {
		name     string
		path     string
		contains string
		seed     func(*Module, *core.Stream)
	}{
		{
			name:     "HLS",
			path:     "/live/delayed-hls.m3u8",
			contains: "#EXTM3U",
			seed: func(m *Module, stream *core.Stream) {
				manager := m.getOrCreateHLS(stream.Key(), stream)
				manager.mu.Lock()
				manager.segments = append(manager.segments, &HLSSegment{SeqNum: 0, Duration: 2, Data: []byte("segment")})
				manager.nextSeqNum = 1
				manager.mu.Unlock()
			},
		},
		{
			name:     "DASH",
			path:     "/live/delayed-dash.mpd",
			contains: "<MPD",
			seed: func(m *Module, stream *core.Stream) {
				manager := m.getOrCreateDASH(stream.Key(), stream)
				manager.mu.Lock()
				manager.videoSegments = append(manager.videoSegments, &DASHSegment{SeqNum: 0, Duration: 2, Data: []byte("segment")})
				manager.nextSeqNum = 1
				manager.videoCodecStr = "avc1.640028"
				manager.mu.Unlock()
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			m, srv, _ := newHTTPTestServer(t)
			streamKey := strings.TrimSuffix(strings.TrimPrefix(test.path, "/"), map[string]string{"HLS": ".m3u8", "DASH": ".mpd"}[test.name])
			stream, err := srv.StreamHub().GetOrCreate(streamKey)
			if err != nil {
				t.Fatal(err)
			}
			if err := stream.SetPublisher(dummyPublisher{}); err != nil {
				t.Fatal(err)
			}

			requestEntered := make(chan struct{}, 1)
			srv.SetAuthorizer(httpTestAuthorizer(func(context.Context, core.AuthorizationRequest) error {
				requestEntered <- struct{}{}
				return nil
			}))
			const shortDeadline = 25 * time.Millisecond
			address := newShortWriteDeadlineServer(t, m.httpSrv.Handler, shortDeadline)

			type result struct {
				status int
				body   string
				err    error
			}
			resultCh := make(chan result, 1)
			go func() {
				response, requestErr := (&http.Client{Timeout: 2 * time.Second}).Get(address + test.path)
				if requestErr != nil {
					resultCh <- result{err: requestErr}
					return
				}
				defer response.Body.Close()
				body, readErr := io.ReadAll(response.Body)
				resultCh <- result{status: response.StatusCode, body: string(body), err: readErr}
			}()

			select {
			case <-requestEntered:
			case <-time.After(time.Second):
				t.Fatal("manifest request did not reach authorization")
			}
			time.Sleep(3 * shortDeadline)
			test.seed(m, stream)

			got := <-resultCh
			if got.err != nil {
				t.Fatal(got.err)
			}
			if got.status != http.StatusOK || !strings.Contains(got.body, test.contains) {
				t.Fatalf("delayed manifest response = status %d body %q", got.status, got.body)
			}
		})
	}
}

func TestHandlerLLHLSInitialPlaylistWaitsForCompletedSegment(t *testing.T) {
	m, srv, addr := newHTTPTestServer(t)
	srv.Config().HTTP.LLHLS.Enabled = true
	srv.Config().HTTP.LLHLS.Container = "fmp4"

	stream, err := srv.StreamHub().GetOrCreate("live/llhls-part")
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.SetPublisher(dummyPublisher{}); err != nil {
		t.Fatal(err)
	}
	mgr := NewLLHLSManager("live/llhls-part", "/live/llhls-part", 0.2, 1.0, 5, "fmp4")
	bindLLHLSManagerToStream(mgr, stream)
	m.llhlsMu.Lock()
	m.llhlsManagers["live/llhls-part"] = mgr
	m.llhlsMu.Unlock()

	type result struct {
		status int
		body   string
		err    error
	}
	resultCh := make(chan result, 1)
	go func() {
		resp, err := (&http.Client{Timeout: 2 * time.Second}).Get(addr + "/live/llhls-part.m3u8")
		if err != nil {
			resultCh <- result{err: err}
			return
		}
		defer resp.Body.Close()
		body, readErr := io.ReadAll(resp.Body)
		resultCh <- result{status: resp.StatusCode, body: string(body), err: readErr}
	}()

	select {
	case got := <-resultCh:
		t.Fatalf("empty initial playlist returned immediately: status %d body %q error %v", got.status, got.body, got.err)
	case <-time.After(100 * time.Millisecond):
	}

	part := &LLHLSPart{
		Index:       0,
		Duration:    0.2,
		Independent: true,
		Data:        []byte("part"),
	}
	mgr.segmenter.callbacks.OnPart(part)

	select {
	case got := <-resultCh:
		t.Fatalf("part-only initial playlist returned to Hls.js: status %d body %q error %v", got.status, got.body, got.err)
	case <-time.After(100 * time.Millisecond):
	}

	mgr.segmenter.callbacks.OnSegment(&LLHLSSegment{
		MSN: 0, Duration: 8.3, Parts: []*LLHLSPart{part},
	})

	var got result
	select {
	case got = <-resultCh:
	case <-time.After(300 * time.Millisecond):
		t.Fatal("initial LL-HLS playlist waited for more than one completed segment")
	}
	if got.err != nil {
		t.Fatal(got.err)
	}
	if got.status != http.StatusOK || !strings.Contains(got.body, "#EXTINF:") {
		t.Fatalf("playlist response = status %d body %q", got.status, got.body)
	}
	if strings.Contains(got.body, "/live/llhls-part/0.0.m4s") {
		t.Fatalf("initial playlist advertised completed segment parts alongside the full segment: %q", got.body)
	}
}

func TestHandlerLLHLSInitialPlaylistNeverReturnsPartsAfterManagerStops(t *testing.T) {
	m, srv, addr := newHTTPTestServer(t)
	srv.Config().HTTP.LLHLS.Enabled = true
	srv.Config().HTTP.LLHLS.Container = "fmp4"
	srv.Config().HTTP.LLHLS.PartDuration = 0.2
	srv.Config().HTTP.LLHLS.SegmentDuration = 15

	stream, err := srv.StreamHub().GetOrCreate("live/llhls-stopped")
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.SetPublisher(dummyPublisher{}); err != nil {
		t.Fatal(err)
	}
	mgr := NewLLHLSManager("live/llhls-stopped", "/live/llhls-stopped", 0.2, 15, 5, "fmp4")
	bindLLHLSManagerToStream(mgr, stream)
	m.llhlsMu.Lock()
	m.llhlsManagers["live/llhls-stopped"] = mgr
	m.llhlsMu.Unlock()

	type result struct {
		status int
		body   string
		err    error
	}
	resultCh := make(chan result, 1)
	go func() {
		resp, err := (&http.Client{Timeout: 2 * time.Second}).Get(addr + "/live/llhls-stopped.m3u8")
		if err != nil {
			resultCh <- result{err: err}
			return
		}
		defer resp.Body.Close()
		body, readErr := io.ReadAll(resp.Body)
		resultCh <- result{status: resp.StatusCode, body: string(body), err: readErr}
	}()

	time.Sleep(50 * time.Millisecond)
	mgr.segmenter.callbacks.OnPart(&LLHLSPart{
		Index:       0,
		Duration:    0.2,
		Independent: true,
		Data:        []byte("part"),
	})
	select {
	case got := <-resultCh:
		t.Fatalf("part-only initial playlist returned before manager stop: status %d body %q error %v", got.status, got.body, got.err)
	case <-time.After(50 * time.Millisecond):
	}

	mgr.Stop()
	select {
	case got := <-resultCh:
		if got.err != nil {
			t.Fatal(got.err)
		}
		if got.status != http.StatusServiceUnavailable {
			t.Fatalf("stopped initial playlist response = status %d body %q, want status %d", got.status, got.body, http.StatusServiceUnavailable)
		}
		if strings.Contains(got.body, "#EXT-X-PART") {
			t.Fatalf("stopped initial playlist returned part-only media: %q", got.body)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("initial playlist did not stop waiting after manager shutdown")
	}
}

func TestHandlerLLHLSInitialPlaylistTimeoutNeverReturnsParts(t *testing.T) {
	m, srv, addr := newHTTPTestServer(t)
	srv.Config().HTTP.LLHLS.Enabled = true
	srv.Config().HTTP.LLHLS.Container = "fmp4"
	srv.Config().HTTP.LLHLS.PartDuration = 0.2
	srv.Config().HTTP.LLHLS.SegmentDuration = 15

	stream, err := srv.StreamHub().GetOrCreate("live/llhls-timeout")
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.SetPublisher(dummyPublisher{}); err != nil {
		t.Fatal(err)
	}
	mgr := NewLLHLSManager("live/llhls-timeout", "/live/llhls-timeout", 0.2, 15, 5, "fmp4")
	bindLLHLSManagerToStream(mgr, stream)
	mgr.initialPlaylistWait = 10 * time.Millisecond
	mgr.segmenter.callbacks.OnPart(&LLHLSPart{
		Index:       0,
		Duration:    0.2,
		Independent: true,
		Data:        []byte("part"),
	})
	m.llhlsMu.Lock()
	m.llhlsManagers["live/llhls-timeout"] = mgr
	m.llhlsMu.Unlock()

	resp, err := (&http.Client{Timeout: time.Second}).Get(addr + "/live/llhls-timeout.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("timed-out initial playlist response = status %d body %q, want status %d", resp.StatusCode, body, http.StatusServiceUnavailable)
	}
	if strings.Contains(string(body), "#EXT-X-PART") {
		t.Fatalf("timed-out initial playlist returned part-only media: %q", body)
	}
}

func bindLLHLSManagerToStream(manager *LLHLSManager, stream *core.Stream) {
	snapshot := stream.StartupSnapshot()
	manager.streamInstanceID = snapshot.StreamInstanceID
	manager.publisherGeneration = snapshot.Generation
	manager.publisherID = snapshot.PublisherID
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

func TestHandlerHLSManifestAndSegmentSupportEscapedDeepStreamKey(t *testing.T) {
	m, srv, addr := newHTTPTestServer(t)
	const streamKey = "tenant/deep/cam?variant#one%raw"
	const escapedBase = "/tenant/deep/cam%3Fvariant%23one%25raw"

	stream, err := srv.StreamHub().GetOrCreate(streamKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.SetPublisher(dummyPublisher{}); err != nil {
		t.Fatal(err)
	}
	mgr := m.getOrCreateHLS(streamKey, stream)
	mgr.mu.Lock()
	mgr.segments = []*HLSSegment{{SeqNum: 0, Duration: 2, Data: []byte("escaped-hls-segment")}}
	mgr.nextSeqNum = 1
	mgr.mu.Unlock()

	resp, err := (&http.Client{Timeout: 2 * time.Second}).Get(addr + escapedBase + ".m3u8")
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("manifest status = %d, want 200: %s", resp.StatusCode, manifest)
	}
	if !strings.Contains(string(manifest), escapedBase+"/0.ts") {
		t.Fatalf("manifest does not preserve escaped deep stream key: %s", manifest)
	}

	resp, err = (&http.Client{Timeout: 2 * time.Second}).Get(addr + escapedBase + "/0.ts")
	if err != nil {
		t.Fatal(err)
	}
	segment, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(segment) != "escaped-hls-segment" {
		t.Fatalf("segment status/body = %d/%q, want 200/%q", resp.StatusCode, segment, "escaped-hls-segment")
	}
}

func TestHandlerDASHManifestAndSegmentsSupportEscapedDeepStreamKey(t *testing.T) {
	m, srv, addr := newHTTPTestServer(t)
	const streamKey = "tenant/deep/cam?variant#one%raw"
	const escapedBase = "/tenant/deep/cam%3Fvariant%23one%25raw"

	stream, err := srv.StreamHub().GetOrCreate(streamKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.SetPublisher(dummyPublisher{}); err != nil {
		t.Fatal(err)
	}
	mgr := m.getOrCreateDASH(streamKey, stream)
	mgr.mu.Lock()
	mgr.videoInitSeg = []byte("escaped-dash-init")
	mgr.videoSegments = []*DASHSegment{{SeqNum: 0, Duration: 2, Data: []byte("escaped-dash-segment")}}
	mgr.nextSeqNum = 1
	mgr.mu.Unlock()

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(addr + escapedBase + ".mpd")
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("manifest status = %d, want 200: %s", resp.StatusCode, manifest)
	}
	for _, want := range []string{escapedBase + "/vinit.mp4", escapedBase + "/v$Number$.m4s"} {
		if !strings.Contains(string(manifest), want) {
			t.Errorf("manifest is missing escaped path %q: %s", want, manifest)
		}
	}

	for path, want := range map[string]string{
		escapedBase + "/vinit.mp4": "escaped-dash-init",
		escapedBase + "/v1.m4s":    "escaped-dash-segment",
	} {
		resp, err = client.Get(addr + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || string(body) != want {
			t.Errorf("GET %s status/body = %d/%q, want 200/%q", path, resp.StatusCode, body, want)
		}
	}
}

func TestHandlerDASHManifestEscapesXMLSensitiveStreamKey(t *testing.T) {
	m, srv, addr := newHTTPTestServer(t)
	const streamKey = "tenant/deep/cam&one"
	const escapedBase = "/tenant/deep/cam&one"

	stream, err := srv.StreamHub().GetOrCreate(streamKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.SetPublisher(dummyPublisher{}); err != nil {
		t.Fatal(err)
	}
	mgr := m.getOrCreateDASH(streamKey, stream)
	mgr.mu.Lock()
	mgr.videoInitSeg = []byte("xml-safe-dash-init")
	mgr.videoSegments = []*DASHSegment{{SeqNum: 0, Duration: 2, Data: []byte("xml-safe-dash-segment")}}
	mgr.nextSeqNum = 1
	mgr.mu.Unlock()

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(addr + escapedBase + ".mpd")
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("manifest status = %d, want 200: %s", resp.StatusCode, manifest)
	}
	var document struct {
		Period struct {
			AdaptationSets []struct {
				SegmentTemplate struct {
					Initialization string `xml:"initialization,attr"`
					Media          string `xml:"media,attr"`
				} `xml:"SegmentTemplate"`
			} `xml:"AdaptationSet"`
		} `xml:"Period"`
	}
	if err := xml.Unmarshal(manifest, &document); err != nil {
		t.Fatalf("parse MPD containing ampersand stream key: %v\n%s", err, manifest)
	}
	if len(document.Period.AdaptationSets) == 0 {
		t.Fatalf("MPD has no adaptation sets: %s", manifest)
	}
	template := document.Period.AdaptationSets[0].SegmentTemplate
	if !strings.HasPrefix(template.Initialization, escapedBase+"/vinit.mp4") || template.Media != escapedBase+"/v$Number$.m4s" {
		t.Fatalf("decoded MPD paths = init %q media %q, want base %q", template.Initialization, template.Media, escapedBase)
	}

	for path, want := range map[string]string{
		template.Initialization:                             "xml-safe-dash-init",
		strings.Replace(template.Media, "$Number$", "1", 1): "xml-safe-dash-segment",
	} {
		resp, err = client.Get(addr + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || string(body) != want {
			t.Errorf("GET %s status/body = %d/%q, want 200/%q", path, resp.StatusCode, body, want)
		}
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
	if cacheControl := resp.Header.Get("Cache-Control"); !strings.Contains(cacheControl, "no-store") {
		t.Errorf("vinit: Cache-Control = %q, want no-store", cacheControl)
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
	if cacheControl := resp.Header.Get("Cache-Control"); !strings.Contains(cacheControl, "no-store") {
		t.Errorf("audio_init: Cache-Control = %q, want no-store", cacheControl)
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
	if cacheControl := resp.Header.Get("Cache-Control"); !strings.Contains(cacheControl, "no-store") {
		t.Errorf("init.mp4: Cache-Control = %q, want no-store", cacheControl)
	}
}

func TestHandlerLLHLSInitIsNotCachedAcrossPublishers(t *testing.T) {
	m, srv, addr := newHTTPTestServer(t)
	srv.Config().HTTP.LLHLS.Enabled = true
	srv.Config().HTTP.LLHLS.Container = "fmp4"

	mgr := NewLLHLSManager("live/llhls-init", "/live/llhls-init", 0.2, 1.0, 5, "fmp4")
	mgr.mu.Lock()
	mgr.initSegment = []byte("llhls-init")
	mgr.mu.Unlock()
	m.llhlsMu.Lock()
	m.llhlsManagers["live/llhls-init"] = mgr
	m.llhlsMu.Unlock()

	resp, err := http.Get(addr + "/live/llhls-init/init.mp4")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if cacheControl := resp.Header.Get("Cache-Control"); !strings.Contains(cacheControl, "no-store") {
		t.Fatalf("Cache-Control = %q, want no-store", cacheControl)
	}
}

func TestHandlerLLHLSInitEpochsRemainBoundToRetainedSegments(t *testing.T) {
	m, srv, addr := newHTTPTestServer(t)
	srv.Config().HTTP.LLHLS.Enabled = true
	srv.Config().HTTP.LLHLS.Container = "fmp4"

	const streamKey = "live/llhls-init-epochs"
	mgr := NewLLHLSManager(streamKey, "/"+streamKey, 0.2, 1.0, 2, "fmp4")
	m.llhlsMu.Lock()
	m.llhlsManagers[streamKey] = mgr
	m.llhlsMu.Unlock()

	publishEpoch := func(msn int, init []byte) string {
		t.Helper()
		part := &LLHLSPart{Index: 0, Duration: 1, Independent: true, Data: []byte{byte(msn)}}
		mgr.segmenter.callbacks.OnInit(init)
		mgr.segmenter.callbacks.OnPart(part)
		mgr.segmenter.callbacks.OnSegment(&LLHLSSegment{
			MSN: msn, Duration: 1, Parts: []*LLHLSPart{part},
		})
		return initSegmentVersion(init)
	}

	oldInit := []byte("immutable-old-init")
	newInit := []byte("immutable-new-init")
	oldVersion := publishEpoch(0, oldInit)
	newVersion := publishEpoch(1, newInit)
	playlist, err := mgr.GeneratePlaylist(t.Context(), -1, -1, false)
	if err != nil {
		t.Fatal(err)
	}
	oldMap := "#EXT-X-MAP:URI=\"/" + streamKey + "/init.mp4?v=" + oldVersion + "\""
	newMap := "#EXT-X-MAP:URI=\"/" + streamKey + "/init.mp4?v=" + newVersion + "\""
	oldMapAt, oldSegmentAt := strings.Index(playlist, oldMap), strings.Index(playlist, "/"+streamKey+"/0.m4s")
	newMapAt, newSegmentAt := strings.Index(playlist, newMap), strings.Index(playlist, "/"+streamKey+"/1.m4s")
	if oldMapAt < 0 || oldSegmentAt < 0 || newMapAt < 0 || newSegmentAt < 0 ||
		oldMapAt > oldSegmentAt || oldSegmentAt > newMapAt || newMapAt > newSegmentAt {
		t.Fatalf("playlist init epoch ordering is invalid:\n%s", playlist)
	}

	getInit := func(version string) (int, []byte) {
		t.Helper()
		resp, err := http.Get(addr + "/" + streamKey + "/init.mp4?v=" + version)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, body
	}
	if status, body := getInit(oldVersion); status != http.StatusOK || !bytes.Equal(body, oldInit) {
		t.Fatalf("old versioned init = status %d body %q, want 200 %q", status, body, oldInit)
	}
	if status, body := getInit(newVersion); status != http.StatusOK || !bytes.Equal(body, newInit) {
		t.Fatalf("new versioned init = status %d body %q, want 200 %q", status, body, newInit)
	}
	if status, _ := getInit("unknown-version"); status != http.StatusNotFound {
		t.Fatalf("unknown versioned init status = %d, want 404", status)
	}

	publishEpoch(2, []byte("immutable-latest-init"))
	if status, _ := getInit(oldVersion); status != http.StatusNotFound {
		t.Fatalf("evicted init epoch status = %d, want 404", status)
	}
	store := reflect.ValueOf(mgr).Elem().FieldByName("initSegments")
	if !store.IsValid() {
		t.Fatal("LL-HLS manager has no bounded init epoch store")
	}
	if got := store.Len(); got > mgr.segmentCount+1 {
		t.Fatalf("retained init epoch count = %d, want at most %d", got, mgr.segmentCount+1)
	}
}

func TestModuleName(t *testing.T) {
	m := NewModule()
	if m.Name() != "httpstream" {
		t.Errorf("expected 'httpstream', got %q", m.Name())
	}
}

func TestRetireManagers(t *testing.T) {
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

	// Retire the managers from lookup without interrupting their producers.
	m.retireManagers("live/cleanup", nil)

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

func (p *mediaPublisher) ID() string                    { return "test-media-pub" }
func (p *mediaPublisher) MediaInfo() *avframe.MediaInfo { return p.info }
func (p *mediaPublisher) Close() error                  { return nil }
