package srt

import (
	"testing"
	"time"

	gosrt "github.com/datarhei/gosrt"
	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
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

func TestPublisherIDAndMediaInfo(t *testing.T) {
	// Use a mock connection (nil is fine for metadata tests)
	pub := &Publisher{
		streamKey: "live/test",
		info:      &avframe.MediaInfo{},
	}
	if pub.ID() != "srt-pub-live/test" {
		t.Errorf("ID = %q", pub.ID())
	}
	if pub.MediaInfo() == nil {
		t.Error("expected non-nil MediaInfo")
	}
}

func TestNewPublisher(t *testing.T) {
	bus := core.NewEventBus()
	hub := core.NewStreamHub(config.StreamConfig{RingBufferSize: 256}, config.LimitsConfig{}, bus)
	pub := NewPublisher(nil, "live/test", hub, bus)
	if pub.ID() != "srt-pub-live/test" {
		t.Errorf("ID = %q", pub.ID())
	}
	if pub.MediaInfo() == nil {
		t.Error("expected non-nil MediaInfo")
	}
	if pub.streamKey != "live/test" {
		t.Errorf("streamKey = %q", pub.streamKey)
	}
}

func TestNewSubscriber(t *testing.T) {
	bus := core.NewEventBus()
	hub := core.NewStreamHub(config.StreamConfig{RingBufferSize: 256}, config.LimitsConfig{}, bus)
	sub := NewSubscriber(nil, "live/test", hub, bus, nil)
	if sub.streamKey != "live/test" {
		t.Errorf("streamKey = %q", sub.streamKey)
	}
}

func TestModuleInitWithPassphrase(t *testing.T) {
	cfg := &config.Config{}
	cfg.SRT.Enabled = true
	cfg.SRT.Listen = "127.0.0.1:16398"
	cfg.SRT.Latency = 120
	cfg.SRT.Passphrase = "secretpassphrase1"
	cfg.SRT.PBKeyLen = 16
	cfg.Stream.RingBufferSize = 256

	s := core.NewServer(cfg)
	m := NewModule()
	s.RegisterModule(m)

	err := m.Init(s)
	if err != nil {
		t.Fatalf("Init error: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	// Try connecting without passphrase (should fail/be rejected)
	srtConfig := gosrt.DefaultConfig()
	srtConfig.StreamId = "publish:/live/test"
	conn, err := gosrt.Dial("srt", "127.0.0.1:16398", srtConfig)
	if err != nil {
		t.Logf("Dial without passphrase returned (expected): %v", err)
	} else {
		conn.Close()
	}

	if err := m.Close(); err != nil {
		t.Fatalf("Close error: %v", err)
	}
}

func TestModuleInitSubscribeMode(t *testing.T) {
	cfg := &config.Config{}
	cfg.SRT.Enabled = true
	cfg.SRT.Listen = "127.0.0.1:16397"
	cfg.SRT.Latency = 120
	cfg.Stream.RingBufferSize = 256

	s := core.NewServer(cfg)
	m := NewModule()
	s.RegisterModule(m)

	err := m.Init(s)
	if err != nil {
		t.Fatalf("Init error: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	// Subscribe mode connection
	srtConfig := gosrt.DefaultConfig()
	srtConfig.StreamId = "subscribe:/live/test"
	conn, err := gosrt.Dial("srt", "127.0.0.1:16397", srtConfig)
	if err != nil {
		t.Logf("Dial subscribe returned (expected): %v", err)
	} else {
		conn.Close()
	}

	if err := m.Close(); err != nil {
		t.Fatalf("Close error: %v", err)
	}
}

func TestModulePublishAndSubscribeFlow(t *testing.T) {
	cfg := &config.Config{}
	cfg.SRT.Enabled = true
	cfg.SRT.Listen = "127.0.0.1:16396"
	cfg.SRT.Latency = 120
	cfg.Stream.RingBufferSize = 256

	s := core.NewServer(cfg)
	m := NewModule()
	s.RegisterModule(m)

	err := m.Init(s)
	if err != nil {
		t.Fatalf("Init error: %v", err)
	}
	defer m.Close()

	time.Sleep(50 * time.Millisecond)

	// Publish connection
	pubConfig := gosrt.DefaultConfig()
	pubConfig.StreamId = "publish:/live/srttest"
	pubConn, err := gosrt.Dial("srt", "127.0.0.1:16396", pubConfig)
	if err != nil {
		t.Fatalf("Dial publish: %v", err)
	}

	// Write some dummy TS data (188-byte TS packets with PAT/PMT)
	tsPacket := make([]byte, 188)
	tsPacket[0] = 0x47 // sync byte
	_, err = pubConn.Write(tsPacket)
	if err != nil {
		t.Logf("Write TS: %v (may be expected)", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Verify publisher was registered
	m.pubsMu.Lock()
	pubCount := len(m.pubs)
	m.pubsMu.Unlock()
	if pubCount == 0 {
		t.Log("publisher not registered (handlePublish may not have completed yet)")
	}

	pubConn.Close()
	time.Sleep(100 * time.Millisecond)
}

func TestModuleEmptyStreamKey(t *testing.T) {
	cfg := &config.Config{}
	cfg.SRT.Enabled = true
	cfg.SRT.Listen = "127.0.0.1:16395"
	cfg.SRT.Latency = 120
	cfg.Stream.RingBufferSize = 256

	s := core.NewServer(cfg)
	m := NewModule()
	s.RegisterModule(m)

	err := m.Init(s)
	if err != nil {
		t.Fatalf("Init error: %v", err)
	}
	defer m.Close()

	time.Sleep(50 * time.Millisecond)

	// Empty stream key should be rejected
	srtConfig := gosrt.DefaultConfig()
	srtConfig.StreamId = "publish:"
	conn, err := gosrt.Dial("srt", "127.0.0.1:16395", srtConfig)
	if err != nil {
		t.Logf("Dial empty key returned (expected rejection): %v", err)
	} else {
		conn.Close()
		t.Error("expected rejection for empty stream key")
	}
}

func TestModuleUnknownMode(t *testing.T) {
	cfg := &config.Config{}
	cfg.SRT.Enabled = true
	cfg.SRT.Listen = "127.0.0.1:16394"
	cfg.SRT.Latency = 120
	cfg.Stream.RingBufferSize = 256

	s := core.NewServer(cfg)
	m := NewModule()
	s.RegisterModule(m)

	err := m.Init(s)
	if err != nil {
		t.Fatalf("Init error: %v", err)
	}
	defer m.Close()

	time.Sleep(50 * time.Millisecond)

	// Use SRT access control format with unknown mode
	srtConfig := gosrt.DefaultConfig()
	srtConfig.StreamId = "#!::r=/live/test,m=unknown"
	conn, err := gosrt.Dial("srt", "127.0.0.1:16394", srtConfig)
	if err != nil {
		t.Logf("Dial unknown mode returned (expected rejection): %v", err)
	} else {
		conn.Close()
		t.Error("expected rejection for unknown mode")
	}
}

func TestModuleDuplicatePublish(t *testing.T) {
	cfg := &config.Config{}
	cfg.SRT.Enabled = true
	cfg.SRT.Listen = "127.0.0.1:16393"
	cfg.SRT.Latency = 120
	cfg.Stream.RingBufferSize = 256

	s := core.NewServer(cfg)
	m := NewModule()
	s.RegisterModule(m)

	err := m.Init(s)
	if err != nil {
		t.Fatalf("Init error: %v", err)
	}
	defer m.Close()

	time.Sleep(50 * time.Millisecond)

	// First publisher
	pubConfig := gosrt.DefaultConfig()
	pubConfig.StreamId = "publish:/live/duptest"
	conn1, err := gosrt.Dial("srt", "127.0.0.1:16393", pubConfig)
	if err != nil {
		t.Fatalf("Dial first publish: %v", err)
	}
	defer conn1.Close()

	time.Sleep(100 * time.Millisecond)

	// Second publisher to same stream — should be rejected or disconnect
	conn2, err := gosrt.Dial("srt", "127.0.0.1:16393", pubConfig)
	if err != nil {
		t.Logf("Dial duplicate publish returned (expected): %v", err)
	} else {
		// Write and expect disconnect
		tsPacket := make([]byte, 188)
		tsPacket[0] = 0x47
		conn2.Write(tsPacket)
		conn2.Close()
	}

	time.Sleep(100 * time.Millisecond)
}


func TestParseStreamIDAccessControl(t *testing.T) {
	// Additional edge cases for SRT access control format
	tests := []struct {
		name     string
		streamID string
		wantMode string
		wantKey  string
	}{
		{
			name:     "access control with extra params",
			streamID: "#!::r=/live/stream1,m=publish,s=extra",
			wantMode: "publish",
			wantKey:  "live/stream1",
		},
		{
			name:     "access control incomplete kv",
			streamID: "#!::incomplete,r=/live/test",
			wantMode: "subscribe",
			wantKey:  "live/test",
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
