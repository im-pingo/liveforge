package httpstream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
)

func TestParseStreamPathWS(t *testing.T) {
	tests := []struct {
		path   string
		app    string
		key    string
		format string
		ok     bool
	}{
		{"/live/test.flv", "live", "test", "flv", true},
		{"/live/test.ts", "live", "test", "ts", true},
		{"/live/test.mp4", "live", "test", "mp4", true},
		{"/live/multi/part.flv", "live", "multi/part", "flv", true},
		{"/noext", "", "", "", false},
		{"/", "", "", "", false},
	}

	for _, tt := range tests {
		app, key, format, ok := parseStreamPath(tt.path)
		if ok != tt.ok || app != tt.app || key != tt.key || format != tt.format {
			t.Errorf("parseStreamPath(%q) = (%q,%q,%q,%v), want (%q,%q,%q,%v)",
				tt.path, app, key, format, ok, tt.app, tt.key, tt.format, tt.ok)
		}
	}
}

// newTestServer creates a minimal Module + core.Server wired for testing.
func newTestServer(t *testing.T) (*Module, *core.Server, string) {
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

	addr := "ws://" + m.Addr().String()
	return m, srv, addr
}

func TestWebSocketContinuousStreamOverwriteClosesTryAgainLater(t *testing.T) {
	for _, format := range []string{"flv", "ts", "mp4"} {
		t.Run(format, func(t *testing.T) {
			buffer := core.NewSharedBuffer(2)
			reader := buffer.NewReader()
			sentinel := []byte("WEBSOCKET-CONTINUITY-GAP-SENTINEL-" + format)
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
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer func() { _ = conn.CloseNow() }()
				if err := writeWebSocketStreamChunk(r.Context(), conn, established, httpStreamWriteTimeout); err != nil {
					return
				}
				serveWebSocketStreamReader(r.Context(), conn, format, "live/overwrite", reader)
			}))
			t.Cleanup(server.Close)

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			conn, resp, err := websocket.Dial(ctx, "ws://"+server.Listener.Addr().String(), nil)
			if resp != nil && resp.Body != nil {
				_ = resp.Body.Close()
			}
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.CloseNow() }()
			messageType, data, err := conn.Read(ctx)
			if err != nil || messageType != websocket.MessageBinary || string(data) != string(established) {
				t.Fatalf("established WebSocket frame = (%v, %q, %v)", messageType, data, err)
			}
			_, data, err = conn.Read(ctx)
			if err == nil {
				t.Fatalf("WebSocket %s received post-gap data %q (sentinel %q)", format, data, sentinel)
			}
			if status := websocket.CloseStatus(err); status != websocket.StatusTryAgainLater {
				t.Fatalf("WebSocket %s close status = %v, want %v (error %v)", format, status, websocket.StatusTryAgainLater, err)
			}
			if !strings.Contains(err.Error(), websocketContinuityLossReason) {
				t.Fatalf("WebSocket %s close error = %q, want bounded continuity reason", format, err)
			}
		})
	}
}

func TestWebSocketCanceledContextWinsOverBufferedOverwrite(t *testing.T) {
	buffer := core.NewSharedBuffer(2)
	reader := buffer.NewReader()
	for _, packet := range [][]byte{
		[]byte("old-0"),
		[]byte("old-1"),
		[]byte("retained-after-gap"),
		[]byte("post-gap-tail"),
	} {
		buffer.Write(packet)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		ctx, cancel := context.WithCancel(r.Context())
		cancel()
		serveWebSocketStreamReader(ctx, conn, "ts", "live/canceled-overwrite", reader)
	}))
	t.Cleanup(server.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, "ws://"+server.Listener.Addr().String(), nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.CloseNow() }()
	_, _, err = conn.Read(ctx)
	if err == nil {
		t.Fatal("canceled WebSocket reader remained open")
	}
	if status := websocket.CloseStatus(err); status != -1 {
		t.Fatalf("canceled WebSocket close status = %v, want no close frame (error %v)", status, err)
	}
}

func TestWebSocketCleanEndUsesNormalClosure(t *testing.T) {
	buffer := core.NewSharedBuffer(2)
	reader := buffer.NewReader()
	buffer.Write([]byte("clean-packet"))
	buffer.Close()
	established := []byte("established-clean")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		if err := writeWebSocketStreamChunk(r.Context(), conn, established, httpStreamWriteTimeout); err != nil {
			return
		}
		serveWebSocketStreamReader(r.Context(), conn, "ts", "live/clean", reader)
	}))
	t.Cleanup(server.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, "ws://"+server.Listener.Addr().String(), nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.CloseNow() }()
	for _, want := range [][]byte{established, []byte("clean-packet")} {
		messageType, data, readErr := conn.Read(ctx)
		if readErr != nil || messageType != websocket.MessageBinary || string(data) != string(want) {
			t.Fatalf("clean WebSocket frame = (%v, %q, %v), want %q", messageType, data, readErr, want)
		}
	}
	_, _, err = conn.Read(ctx)
	if status := websocket.CloseStatus(err); status != websocket.StatusNormalClosure {
		t.Fatalf("clean WebSocket close status = %v, want %v (error %v)", status, websocket.StatusNormalClosure, err)
	}
}

func TestWebSocketUpgrade(t *testing.T) {
	_, srv, addr := newTestServer(t)

	// Create a publishing stream
	stream, err := srv.StreamHub().GetOrCreate("live/test")
	if err != nil {
		t.Fatal(err)
	}
	if publisherErr := stream.SetPublisher(dummyPublisher{}); publisherErr != nil {
		t.Fatal(publisherErr)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	conn, resp, err := websocket.Dial(ctx, addr+"/ws/live/test.ts", nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("websocket dial: %v", err)
	}
	defer func() { _ = conn.CloseNow() }()

	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Errorf("expected 101, got %d", resp.StatusCode)
	}

	// Close cleanly
	conn.Close(websocket.StatusNormalClosure, "done")
}

func TestModuleCloseTerminatesActiveWebSocketSubscriber(t *testing.T) {
	m, srv, addr := newTestServer(t)
	stream, err := srv.StreamHub().GetOrCreate("live/close-active-ws")
	if err != nil {
		t.Fatal(err)
	}
	if publisherErr := stream.SetPublisher(dummyPublisher{}); publisherErr != nil {
		t.Fatal(publisherErr)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, addr+"/ws/live/close-active-ws.ts", nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("websocket dial: %v", err)
	}
	defer func() { _ = conn.CloseNow() }()
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
		t.Fatal("HTTP module close exceeded the active WebSocket drain bound")
	}
	if got := srv.ConnectionCount(); got != 0 {
		t.Fatalf("connection count after HTTP module close = %d, want 0", got)
	}
}

func TestWebSocketInvalidFormat(t *testing.T) {
	_, _, addr := newTestServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, _, err := websocket.Dial(ctx, addr+"/ws/live/test.mkv", nil)
	if err == nil {
		t.Fatal("expected error for unsupported format")
	}
}

func TestWebSocketStreamNotFound(t *testing.T) {
	_, _, addr := newTestServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, _, err := websocket.Dial(ctx, addr+"/ws/live/nonexist.flv", nil)
	if err == nil {
		t.Fatal("expected error for missing stream")
	}
}

func TestWebSocketBinaryFrames(t *testing.T) {
	m, srv, addr := newTestServer(t)

	// Create a publishing stream with muxer callback that writes test data
	stream, err := srv.StreamHub().GetOrCreate("live/frames")
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.SetPublisher(dummyPublisher{}); err != nil {
		t.Fatal(err)
	}

	// Pre-mark as registered so ensureMuxerCallbacks won't overwrite our test callback
	m.registeredMu.Lock()
	m.registered[stream.Key()] = stream.InstanceID()
	m.registeredMu.Unlock()

	// Register a simple muxer start callback that pushes test data
	mm := stream.MuxerManager()
	mm.RegisterMuxerStart("ts", func(inst *core.MuxerInstance, s *core.Stream) {
		go func() {
			defer inst.Buffer.Close()
			inst.Buffer.Write([]byte("frame-1"))
			inst.Buffer.Write([]byte("frame-2"))
		}()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, addr+"/ws/live/frames.ts", nil)
	if err != nil {
		t.Fatalf("websocket dial: %v", err)
	}
	defer conn.CloseNow()

	// Read two binary frames
	for _, expected := range []string{"frame-1", "frame-2"} {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if typ != websocket.MessageBinary {
			t.Errorf("expected binary message, got %v", typ)
		}
		if string(data) != expected {
			t.Errorf("got %q, want %q", data, expected)
		}
	}
}

func TestWebSocketInvalidPath(t *testing.T) {
	_, _, addr := newTestServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, _, err := websocket.Dial(ctx, addr+"/ws/badpath", nil)
	if err == nil {
		t.Fatal("expected error for invalid path")
	}
}

// dummyPublisher satisfies the core.Publisher interface for testing.
type dummyPublisher struct{}

func (dummyPublisher) ID() string                    { return "test-pub" }
func (dummyPublisher) MediaInfo() *avframe.MediaInfo { return nil }
func (dummyPublisher) Close() error                  { return nil }
