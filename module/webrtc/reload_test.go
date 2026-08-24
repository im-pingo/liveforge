package webrtc

import (
	"net/http"
	"testing"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
)

func TestGCCConfigReadsCurrentSnapshotForNewSessions(t *testing.T) {
	cfg := config.Defaults()
	server := core.NewServer(cfg)
	m := NewModule()
	m.server = server

	next := *cfg
	next.WebRTC.GCC.InitialBitrate = 765432
	next.WebRTC.GCC.MinBitrate = 12345
	next.WebRTC.GCC.MaxBitrate = 876543
	server.UpdateConfig(&next)
	if err := m.OnReload(server); err != nil {
		t.Fatal(err)
	}
	got := m.gccConfig()
	if got.InitialBitrate != 765432 || got.MinBitrate != 12345 || got.MaxBitrate != 876543 {
		t.Fatalf("GCC config = %#v", got)
	}
}

func TestOnReloadReplacesSignalingRateLimiter(t *testing.T) {
	cfg := config.Defaults()
	cfg.WebRTC.Listen = "127.0.0.1:0"
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

	client := &http.Client{}
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequest(http.MethodOptions, "http://"+m.Addr().String()+"/webrtc/whep/live/test", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if attempt == 0 && resp.StatusCode == http.StatusTooManyRequests {
			t.Fatalf("first request was rate limited")
		}
		if attempt == 1 && resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("second request status=%d want=429", resp.StatusCode)
		}
	}
}
