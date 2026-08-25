package cluster

import (
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
)

func TestModuleOnReloadUpdatesSchedulingRetryAndHealthPolicy(t *testing.T) {
	cfg := config.Defaults()
	cfg.Cluster.Forward.Enabled = true
	cfg.Cluster.Forward.Targets = []string{"rtmp://old/live"}
	cfg.Cluster.HealthCheck.Enabled = true
	cfg.Cluster.HealthCheck.EvictThreshold = 4
	server := core.NewServer(cfg)
	m := NewModule()
	if err := m.Init(server); err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	next := *cfg
	next.Cluster.Forward.Targets = []string{"rtmp://new/live"}
	next.Cluster.Forward.RetryMax = 9
	next.Cluster.Forward.RetryInterval = 250 * time.Millisecond
	next.Cluster.HealthCheck.EvictThreshold = 7
	server.UpdateConfig(&next)
	if err := m.OnReload(server); err != nil {
		t.Fatal(err)
	}
	targets, err := m.forward.scheduler.Resolve("forward", "live/test")
	if err != nil || len(targets) != 1 || targets[0] != "rtmp://new/live" {
		t.Fatalf("targets=%v err=%v", targets, err)
	}
	if m.forward.retryMax != 9 || m.forward.retryDel != 250*time.Millisecond {
		t.Fatalf("retry policy=%d/%v", m.forward.retryMax, m.forward.retryDel)
	}
	if m.health.evictThresholdValue() != 7 {
		t.Fatalf("evict threshold=%d", m.health.evictThresholdValue())
	}
}
