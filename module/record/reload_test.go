package record

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
)

func TestModuleOnReloadAppliesPolicyToNewSessions(t *testing.T) {
	first := &config.Config{Record: config.RecordConfig{Enabled: true, StreamPattern: "live/*", Format: "flv", Path: filepath.Join(t.TempDir(), "{stream_key}.flv")}}
	server := core.NewServer(first)
	m := NewModule()
	if err := m.Init(server); err != nil {
		t.Fatal(err)
	}

	next := *first
	next.Record.StreamPattern = "camera/*"
	next.Record.Segment.Duration = time.Minute
	next.Record.Enabled = false
	server.UpdateConfig(&next)
	if err := m.OnReload(server); err != nil {
		t.Fatal(err)
	}
	policy := m.Policy()
	if policy.StreamPattern != "camera/*" || policy.Segment.Duration != time.Minute {
		t.Fatalf("policy = %+v", policy)
	}
	if !policy.Enabled {
		t.Fatal("enabled is process lifecycle state and must remain active until restart")
	}
}
