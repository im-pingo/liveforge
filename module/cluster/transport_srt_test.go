package cluster

import (
	"context"
	"testing"
)

func TestSRTTransportScheme(t *testing.T) {
	cfg := defaultClusterSRTConfig()
	tr := NewSRTTransport(cfg)
	if tr.Scheme() != "srt" {
		t.Errorf("Scheme = %q, want %q", tr.Scheme(), "srt")
	}
}

func TestSRTTransportPushBadURL(t *testing.T) {
	cfg := defaultClusterSRTConfig()
	tr := NewSRTTransport(cfg)
	defer tr.Close()

	hub, _ := newTestHub()
	stream, _ := hub.GetOrCreate("live/test")

	ctx := context.Background()
	err := tr.Push(ctx, "srt://127.0.0.1:19999/live/test", stream)
	// Should fail to connect (no SRT server at :19999)
	if err == nil {
		t.Error("expected error for connection to non-existent server")
	}
}

func TestParseSRTURL(t *testing.T) {
	tests := []struct {
		url      string
		addr     string
		streamID string
		wantErr  bool
	}{
		{"srt://host:6000/live/test", "host:6000", "/live/test", false},
		{"srt://host/live/test", "host:6000", "/live/test", false},
		{"srt://host:9000?streamid=/live/test", "host:9000", "/live/test", false},
		{"srt://host", "", "", true},           // no path
		{"rtmp://host/live/test", "", "", true}, // wrong scheme
	}
	for _, tt := range tests {
		addr, streamID, err := parseSRTURL(tt.url)
		if (err != nil) != tt.wantErr {
			t.Errorf("parseSRTURL(%q) err=%v, wantErr=%v", tt.url, err, tt.wantErr)
			continue
		}
		if !tt.wantErr {
			if addr != tt.addr {
				t.Errorf("parseSRTURL(%q) addr=%q, want %q", tt.url, addr, tt.addr)
			}
			if streamID != tt.streamID {
				t.Errorf("parseSRTURL(%q) streamID=%q, want %q", tt.url, streamID, tt.streamID)
			}
		}
	}
}

func TestSRTTransportInterfaceCompliance(t *testing.T) {
	cfg := defaultClusterSRTConfig()
	var _ RelayTransport = NewSRTTransport(cfg)
}
