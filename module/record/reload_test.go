package record

import (
	"context"
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

func TestModuleOnReloadKeepsStoragePathAndPriorRecordingsManageable(t *testing.T) {
	firstRoot := t.TempDir()
	secondRoot := t.TempDir()
	first := &config.Config{Record: config.RecordConfig{Enabled: true, Format: "flv", Path: filepath.Join(firstRoot, "{stream_key}.flv")}}
	server := core.NewServer(first)
	m := NewModule()
	if err := m.Init(server); err != nil {
		t.Fatal(err)
	}
	runtime := m.runtime.Load()
	object, err := runtime.storage.Create(context.Background(), "live/prior.flv", RecordingInfo{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := object.Write([]byte("prior")); err != nil {
		t.Fatal(err)
	}
	if _, err := object.Complete(context.Background(), RecordingInfo{}); err != nil {
		t.Fatal(err)
	}

	next := *first
	next.Record.Path = filepath.Join(secondRoot, "{stream_key}.flv")
	server.UpdateConfig(&next)
	if err := m.OnReload(server); err != nil {
		t.Fatal(err)
	}
	if got := m.Policy().Path; got != first.Record.Path {
		t.Fatalf("record path changed in place: %q", got)
	}
	items, err := m.ListRecordings(context.Background())
	if err != nil || len(items) != 1 || items[0].ID != "live/prior.flv" {
		t.Fatalf("prior recordings disappeared: items=%+v err=%v", items, err)
	}
}
