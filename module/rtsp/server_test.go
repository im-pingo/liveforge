package rtsp

import (
	"testing"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
)

func TestModuleInterface(t *testing.T) {
	m := NewModule()
	var _ core.Module = m

	if m.Name() != "rtsp" {
		t.Errorf("Name = %q, want %q", m.Name(), "rtsp")
	}
	if hooks := m.Hooks(); hooks != nil {
		t.Errorf("Hooks should be nil, got %v", hooks)
	}
}

func TestGenerateSessionID(t *testing.T) {
	id1 := generateSessionID()
	id2 := generateSessionID()
	if len(id1) != 16 { // 8 bytes = 16 hex chars
		t.Errorf("session ID length = %d, want 16", len(id1))
	}
	if id1 == id2 {
		t.Error("two session IDs should be different")
	}
}

func TestModuleCloseCleansActiveSessions(t *testing.T) {
	cfg := &config.Config{Stream: config.StreamConfig{RingBufferSize: 16}}
	server := core.NewServer(cfg)
	stream, err := server.StreamHub().GetOrCreate("live/shutdown")
	if err != nil {
		t.Fatal(err)
	}
	pub, err := NewRTSPPublisherWithTracks("publisher", &avframe.MediaInfo{VideoCodec: avframe.CodecH264}, stream, nil)
	if err != nil {
		t.Fatal(err)
	}
	generation, err := stream.SetPublisherWithGeneration(pub)
	if err != nil {
		t.Fatal(err)
	}
	session := NewRTSPSession("session-1", "live/shutdown")
	session.Stream = stream
	session.Publisher = pub
	session.PublisherGeneration = generation
	m := NewModule()
	m.server = server
	m.sessions[session.ID] = session

	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if stream.Publisher() != nil {
		t.Fatal("module close left RTSP publisher attached")
	}
	if len(m.sessions) != 0 {
		t.Fatalf("active sessions after close = %d, want 0", len(m.sessions))
	}
}
