package rtsp

import (
	"testing"

	"github.com/im-pingo/liveforge/core"
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
