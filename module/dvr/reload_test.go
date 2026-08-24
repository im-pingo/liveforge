package dvr

import (
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
)

func TestModuleOnReloadAppliesCleanupPolicy(t *testing.T) {
	initial := &config.Config{DVR: config.DVRConfig{Enabled: true, Listen: ":10000", Window: time.Hour, CleanupInterval: time.Minute}}
	server := core.NewServer(initial)
	m := NewModule()
	m.server = server
	m.storePolicy(initial.DVR)

	next := *initial
	next.DVR.Window = 10 * time.Minute
	next.DVR.CleanupInterval = 5 * time.Second
	next.DVR.Enabled = false
	next.DVR.Listen = ":20000"
	server.UpdateConfig(&next)
	if err := m.OnReload(server); err != nil {
		t.Fatal(err)
	}
	policy := m.Policy()
	if policy.Window != 10*time.Minute || policy.CleanupInterval != 5*time.Second {
		t.Fatalf("policy = %+v", policy)
	}
	if !policy.Enabled || policy.Listen != ":10000" {
		t.Fatalf("restart-required policy changed in place: %+v", policy)
	}
}
