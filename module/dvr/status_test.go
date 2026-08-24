package dvr

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
)

func TestModuleDVRStatusAndDetail(t *testing.T) {
	dir := t.TempDir()
	hub := core.NewStreamHub(config.StreamConfig{RingBufferSize: 16}, config.LimitsConfig{}, core.NewEventBus())
	stream, _ := hub.GetOrCreate("live/status")
	session, err := NewSession("live/status", stream, config.DVRConfig{Path: filepath.Join(dir, "{stream_key}")}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	session.Index().Add(Segment{SeqNum: 1, StartTime: time.Now(), Duration: 5, Size: 12})
	m := NewModule()
	m.storePolicy(config.DVRConfig{Path: filepath.Join(dir, "{stream_key}")})
	m.sessions["live/status"] = session

	status := m.DVRStatus()
	if len(status.Sessions) != 1 || status.Sessions[0].Bytes != 12 || !status.Storage.Healthy {
		t.Fatalf("status = %+v", status)
	}
	detail, ok := m.DVRSession("live/status")
	if !ok || detail.Duration != 5 || detail.Segments != 1 {
		t.Fatalf("detail=%+v ok=%v", detail, ok)
	}
	if _, ok := m.DVRSession("missing"); ok {
		t.Fatal("missing session found")
	}
}

func TestModuleCleanupMetrics(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "expired.ts")
	if err := os.WriteFile(path, []byte("expired"), 0644); err != nil {
		t.Fatal(err)
	}
	hub := core.NewStreamHub(config.StreamConfig{RingBufferSize: 16}, config.LimitsConfig{}, core.NewEventBus())
	stream, _ := hub.GetOrCreate("live/cleanup")
	session, err := NewSession("live/cleanup", stream, config.DVRConfig{Path: filepath.Join(dir, "{stream_key}")}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	session.Stop()
	session.Index().Add(Segment{SeqNum: 1, StartTime: time.Now().Add(-time.Hour), Size: 7, DiskPath: path})
	m := NewModule()
	m.storePolicy(config.DVRConfig{Path: filepath.Join(dir, "{stream_key}"), Window: time.Minute})
	m.sessions["live/cleanup"] = session

	m.cleanExpiredSegments()
	metrics := m.DVRStatus().Metrics
	if metrics.CleanupDeleted != 1 || metrics.CleanupBytes != 7 || metrics.CleanupFailures != 0 {
		t.Fatalf("metrics = %+v", metrics)
	}
}
