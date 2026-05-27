package cluster

import (
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
)

func TestHealthTrackerRecordSuccessClearsFailures(t *testing.T) {
	ht := NewHealthTracker(config.HealthCheckConfig{
		Enabled:        true,
		EvictThreshold: 3,
		Interval:       time.Hour,
		Timeout:        time.Second,
	})
	defer ht.Close()

	ht.RecordFailure("rtmp://node1:1935/live")
	ht.RecordFailure("rtmp://node1:1935/live")
	ht.RecordSuccess("rtmp://node1:1935/live")

	snap := ht.Snapshot()
	ns := snap["node1:1935"]
	if ns.ConsecutiveFailures != 0 {
		t.Errorf("ConsecutiveFailures = %d, want 0", ns.ConsecutiveFailures)
	}
	if ns.Evicted {
		t.Error("should not be evicted after success")
	}
}

func TestHealthTrackerEviction(t *testing.T) {
	ht := NewHealthTracker(config.HealthCheckConfig{
		Enabled:        true,
		EvictThreshold: 2,
		Interval:       time.Hour,
		Timeout:        time.Second,
	})
	defer ht.Close()

	evicted := ht.RecordFailure("rtmp://node1:1935/live")
	if evicted {
		t.Error("should not evict after 1 failure")
	}

	evicted = ht.RecordFailure("rtmp://node1:1935/live")
	if !evicted {
		t.Error("should evict after 2 failures (threshold=2)")
	}

	if !ht.IsEvicted("rtmp://node1:1935/live") {
		t.Error("IsEvicted should return true")
	}
}

func TestHealthTrackerFilterHealthy(t *testing.T) {
	ht := NewHealthTracker(config.HealthCheckConfig{
		Enabled:        true,
		EvictThreshold: 1,
		Interval:       time.Hour,
		Timeout:        time.Second,
	})
	defer ht.Close()

	ht.RecordFailure("rtmp://bad:1935/live")

	urls := []string{
		"rtmp://good:1935/live",
		"rtmp://bad:1935/live",
		"rtmp://also-good:1935/live",
	}

	healthy := ht.FilterHealthy(urls)
	if len(healthy) != 2 {
		t.Fatalf("FilterHealthy = %d URLs, want 2", len(healthy))
	}
	if healthy[0] != "rtmp://good:1935/live" || healthy[1] != "rtmp://also-good:1935/live" {
		t.Errorf("unexpected healthy list: %v", healthy)
	}
}

func TestHealthTrackerSuccessRecovery(t *testing.T) {
	ht := NewHealthTracker(config.HealthCheckConfig{
		Enabled:        true,
		EvictThreshold: 1,
		Interval:       time.Hour,
		Timeout:        time.Second,
	})
	defer ht.Close()

	ht.RecordFailure("rtmp://node1:1935/live")
	if !ht.IsEvicted("rtmp://node1:1935/live") {
		t.Fatal("should be evicted")
	}

	ht.RecordSuccess("rtmp://node1:1935/live")
	if ht.IsEvicted("rtmp://node1:1935/live") {
		t.Error("should recover after success")
	}
}

func TestHealthTrackerExtractHost(t *testing.T) {
	tests := []struct {
		url  string
		want string
	}{
		{"rtmp://host:1935/live/stream", "host:1935"},
		{"srt://192.168.1.1:6000", "192.168.1.1:6000"},
		{"rtsp://node:554/live/test", "node:554"},
		{"invalid-url", ""},
	}

	for _, tt := range tests {
		got := extractHost(tt.url)
		if got != tt.want {
			t.Errorf("extractHost(%q) = %q, want %q", tt.url, got, tt.want)
		}
	}
}

func TestHealthTrackerUnknownURLNotEvicted(t *testing.T) {
	ht := NewHealthTracker(config.HealthCheckConfig{
		Enabled:        true,
		EvictThreshold: 3,
		Interval:       time.Hour,
		Timeout:        time.Second,
	})
	defer ht.Close()

	if ht.IsEvicted("rtmp://never-seen:1935/live") {
		t.Error("unknown host should not be evicted")
	}
}

func TestHealthTrackerSnapshot(t *testing.T) {
	ht := NewHealthTracker(config.HealthCheckConfig{
		Enabled:        true,
		EvictThreshold: 3,
		Interval:       time.Hour,
		Timeout:        time.Second,
	})
	defer ht.Close()

	ht.RecordSuccess("rtmp://node1:1935/live")
	ht.RecordFailure("rtmp://node2:1935/live")

	snap := ht.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("Snapshot has %d entries, want 2", len(snap))
	}
	if snap["node1:1935"].ConsecutiveFailures != 0 {
		t.Error("node1 should have 0 failures")
	}
	if snap["node2:1935"].ConsecutiveFailures != 1 {
		t.Error("node2 should have 1 failure")
	}
}

func TestHealthTrackerCloseIdempotent(t *testing.T) {
	ht := NewHealthTracker(config.HealthCheckConfig{
		Enabled:        true,
		EvictThreshold: 3,
		Interval:       time.Hour,
		Timeout:        time.Second,
	})
	ht.Close()
	ht.Close() // should not panic
}
