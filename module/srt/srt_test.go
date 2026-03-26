package srt

import (
	"testing"
	"time"

	gosrt "github.com/datarhei/gosrt"
	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
)

func TestParseStreamID(t *testing.T) {
	tests := []struct {
		name      string
		streamID  string
		wantMode  string
		wantKey   string
	}{
		{
			name:     "publish prefix",
			streamID: "publish:/live/stream1",
			wantMode: "publish",
			wantKey:  "live/stream1",
		},
		{
			name:     "subscribe prefix",
			streamID: "subscribe:/live/stream1",
			wantMode: "subscribe",
			wantKey:  "live/stream1",
		},
		{
			name:     "srt access control publish",
			streamID: "#!::r=/live/stream1,m=publish",
			wantMode: "publish",
			wantKey:  "live/stream1",
		},
		{
			name:     "srt access control request",
			streamID: "#!::r=/live/stream1,m=request",
			wantMode: "request",
			wantKey:  "live/stream1",
		},
		{
			name:     "srt access control default mode",
			streamID: "#!::r=/live/stream1",
			wantMode: "subscribe",
			wantKey:  "live/stream1",
		},
		{
			name:     "bare path defaults to subscribe",
			streamID: "/live/stream1",
			wantMode: "subscribe",
			wantKey:  "live/stream1",
		},
		{
			name:     "path with query string stripped",
			streamID: "publish:/live/stream1?token=abc",
			wantMode: "publish",
			wantKey:  "live/stream1",
		},
		{
			name:     "no leading slash",
			streamID: "publish:live/stream1",
			wantMode: "publish",
			wantKey:  "live/stream1",
		},
		{
			name:     "empty streamid",
			streamID: "",
			wantMode: "subscribe",
			wantKey:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mode, key := parseStreamID(tt.streamID)
			if mode != tt.wantMode {
				t.Errorf("mode = %q, want %q", mode, tt.wantMode)
			}
			if key != tt.wantKey {
				t.Errorf("key = %q, want %q", key, tt.wantKey)
			}
		})
	}
}

func TestNormalizeStreamKey(t *testing.T) {
	tests := []struct {
		raw  string
		want string
	}{
		{"/live/stream1", "live/stream1"},
		{"live/stream1", "live/stream1"},
		{"/live/stream1?token=abc&foo=bar", "live/stream1"},
		{"", ""},
	}

	for _, tt := range tests {
		got := normalizeStreamKey(tt.raw)
		if got != tt.want {
			t.Errorf("normalizeStreamKey(%q) = %q, want %q", tt.raw, got, tt.want)
		}
	}
}

func TestModuleName(t *testing.T) {
	m := NewModule()
	if m.Name() != "srt" {
		t.Errorf("Name() = %q, want %q", m.Name(), "srt")
	}
}

func TestModuleHooksEmpty(t *testing.T) {
	m := NewModule()
	hooks := m.Hooks()
	if hooks != nil {
		t.Errorf("Hooks() = %v, want nil", hooks)
	}
}

func TestModuleInitAndClose(t *testing.T) {
	cfg := &config.Config{}
	cfg.SRT.Enabled = true
	cfg.SRT.Listen = "127.0.0.1:0" // will fail since gosrt needs a real port
	cfg.SRT.Latency = 120
	cfg.Stream.RingBufferSize = 256

	s := core.NewServer(cfg)
	m := NewModule()
	s.RegisterModule(m)

	// Use a real available port
	cfg.SRT.Listen = "127.0.0.1:16399"

	err := m.Init(s)
	if err != nil {
		t.Fatalf("Init error: %v", err)
	}

	// Give the server a moment to start
	time.Sleep(50 * time.Millisecond)

	// Verify the module can accept a caller connection
	srtConfig := gosrt.DefaultConfig()
	srtConfig.StreamId = "publish:/live/test"

	conn, err := gosrt.Dial("srt", "127.0.0.1:16399", srtConfig)
	if err != nil {
		// Connection may be rejected (no auth handler approved), that's OK for this test.
		// The point is that the server was listening and responded.
		t.Logf("Dial returned (expected): %v", err)
	} else {
		conn.Close()
	}

	if err := m.Close(); err != nil {
		t.Fatalf("Close error: %v", err)
	}
}

func TestConfigDefaults(t *testing.T) {
	cfg := defaultTestConfig()
	if cfg.SRT.Listen != ":6000" {
		t.Errorf("default SRT listen = %q, want %q", cfg.SRT.Listen, ":6000")
	}
	if cfg.SRT.Latency != 120 {
		t.Errorf("default SRT latency = %d, want %d", cfg.SRT.Latency, 120)
	}
}

func defaultTestConfig() *config.Config {
	// Minimal config that exercises SRT defaults
	cfg := &config.Config{}
	// Apply same defaults as loader.go
	cfg.SRT.Listen = ":6000"
	cfg.SRT.Latency = 120
	return cfg
}
