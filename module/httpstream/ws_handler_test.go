package httpstream

import (
	"context"
	"net/http"
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

	cfg := &config.Config{}
	cfg.HTTP.Listen = "127.0.0.1:0"
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

func TestWebSocketUpgrade(t *testing.T) {
	_, srv, addr := newTestServer(t)

	// Create a publishing stream
	stream, err := srv.StreamHub().GetOrCreate("live/test")
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.SetPublisher(dummyPublisher{}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	conn, resp, err := websocket.Dial(ctx, addr+"/ws/live/test.ts", nil)
	if err != nil {
		t.Fatalf("websocket dial: %v", err)
	}
	defer conn.CloseNow()

	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Errorf("expected 101, got %d", resp.StatusCode)
	}

	// Close cleanly
	conn.Close(websocket.StatusNormalClosure, "done")
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
	m.registered[stream] = true
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

func (dummyPublisher) ID() string                   { return "test-pub" }
func (dummyPublisher) MediaInfo() *avframe.MediaInfo { return nil }
func (dummyPublisher) Close() error                  { return nil }
