package httpstream

import (
	"net/http"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
)

func TestOnReloadStopsSegmentManagersSoNewPolicyIsUsed(t *testing.T) {
	cfg := config.Defaults()
	server := core.NewServer(cfg)
	m := NewModule()
	m.server = server
	hls := NewHLSManager("live/test", "/live/test", 6, 5)
	dash := NewDASHManager("live/test", "/live/test", 6, 5)
	llhls := NewLLHLSManager("live/test", "/live/test", 0.2, 4, "fmp4")
	m.hlsManagers["live/test"] = hls
	m.dashManagers["live/test"] = dash
	m.llhlsManagers["live/test"] = llhls

	next := *cfg
	next.HTTP.HLS.SegmentDuration = 2
	next.HTTP.DASH.SegmentDuration = 3
	next.HTTP.LLHLS.PartDuration = 0.5
	server.UpdateConfig(&next)
	if err := m.OnReload(server); err != nil {
		t.Fatal(err)
	}
	if len(m.hlsManagers) != 0 || len(m.dashManagers) != 0 || len(m.llhlsManagers) != 0 {
		t.Fatal("segment managers using stale policy were retained")
	}
	select {
	case <-hls.done:
	case <-time.After(time.Second):
		t.Fatal("HLS manager was not stopped")
	}
}

func TestOnReloadReplacesMediaRateLimiter(t *testing.T) {
	cfg := config.Defaults()
	cfg.HTTP.Listen = "127.0.0.1:0"
	cfg.Limits.RateLimit.Enabled = false
	server := core.NewServer(cfg)
	m := NewModule()
	server.RegisterModule(m)
	if err := server.Init(); err != nil {
		t.Fatal(err)
	}
	defer server.Shutdown()

	next := *cfg
	next.Limits.RateLimit.Enabled = true
	next.Limits.RateLimit.Rate = 0.001
	next.Limits.RateLimit.Burst = 1
	server.UpdateConfig(&next)

	for attempt := 0; attempt < 2; attempt++ {
		resp, err := http.Get("http://" + m.Addr().String() + "/missing.flv")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if attempt == 0 && resp.StatusCode == http.StatusTooManyRequests {
			t.Fatal("first request was rate limited")
		}
		if attempt == 1 && resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("second request status=%d want=429", resp.StatusCode)
		}
	}
}

func TestOnReloadKeepsSegmentManagersForUnrelatedConfigChange(t *testing.T) {
	cfg := config.Defaults()
	server := core.NewServer(cfg)
	m := NewModule()
	m.server = server
	m.policy = cfg.HTTP
	hls := NewHLSManager("live/test", "/live/test", 6, 5)
	m.hlsManagers["live/test"] = hls

	next := *cfg
	next.Auth.Enabled = !cfg.Auth.Enabled
	server.UpdateConfig(&next)
	if err := m.OnReload(server); err != nil {
		t.Fatal(err)
	}
	if m.hlsManagers["live/test"] != hls {
		t.Fatal("unrelated reload removed active HLS manager")
	}
}
