package testutil

import (
	"net"
	"net/http"
	"testing"
	"time"
)

func TestStartTestServer_RTMPOnly(t *testing.T) {
	srv := StartTestServer(t, WithRTMP())

	if addr := srv.RTMPAddr(); addr == "" {
		t.Fatal("RTMPAddr returned empty string")
	}
	if addr := srv.RTSPAddr(); addr != "" {
		t.Errorf("RTSPAddr should be empty when RTSP not enabled, got %q", addr)
	}
	if addr := srv.HTTPAddr(); addr != "" {
		t.Errorf("HTTPAddr should be empty when HTTP not enabled, got %q", addr)
	}
	if addr := srv.APIAddr(); addr != "" {
		t.Errorf("APIAddr should be empty when API not enabled, got %q", addr)
	}
}

func TestStartTestServer_MultipleModules(t *testing.T) {
	srv := StartTestServer(t, WithRTMP(), WithHTTPStream(), WithAPI())

	if addr := srv.RTMPAddr(); addr == "" {
		t.Fatal("RTMPAddr returned empty string")
	}
	if addr := srv.HTTPAddr(); addr == "" {
		t.Fatal("HTTPAddr returned empty string")
	}
	if addr := srv.APIAddr(); addr == "" {
		t.Fatal("APIAddr returned empty string")
	}
	// Disabled modules should return empty.
	if addr := srv.RTSPAddr(); addr != "" {
		t.Errorf("RTSPAddr should be empty, got %q", addr)
	}
	if addr := srv.SRTAddr(); addr != "" {
		t.Errorf("SRTAddr should be empty, got %q", addr)
	}
	if addr := srv.WebRTCAddr(); addr != "" {
		t.Errorf("WebRTCAddr should be empty, got %q", addr)
	}
}

func TestStartTestServer_HTTPReachable(t *testing.T) {
	srv := StartTestServer(t, WithHTTPStream())

	addr := srv.HTTPAddr()
	if addr == "" {
		t.Fatal("HTTPAddr returned empty string")
	}

	// Verify the HTTP server is actually listening by making a request.
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + addr + "/nonexistent.flv")
	if err != nil {
		t.Fatalf("HTTP request to %s failed: %v", addr, err)
	}
	resp.Body.Close()
	// We don't care about the status code, just that the server responded.
}

func TestStartTestServer_APIReachable(t *testing.T) {
	srv := StartTestServer(t, WithAPI())

	addr := srv.APIAddr()
	if addr == "" {
		t.Fatal("APIAddr returned empty string")
	}

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + addr + "/api/streams")
	if err != nil {
		t.Fatalf("HTTP request to %s failed: %v", addr, err)
	}
	resp.Body.Close()
}

func TestStartTestServer_RTMPListening(t *testing.T) {
	srv := StartTestServer(t, WithRTMP())

	addr := srv.RTMPAddr()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to RTMP at %s: %v", addr, err)
	}
	conn.Close()
}

func TestStartTestServer_RTSPListening(t *testing.T) {
	srv := StartTestServer(t, WithRTSP())

	addr := srv.RTSPAddr()
	if addr == "" {
		t.Fatal("RTSPAddr returned empty string")
	}

	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to RTSP at %s: %v", addr, err)
	}
	conn.Close()
}

func TestStartTestServer_ConfigReturned(t *testing.T) {
	srv := StartTestServer(t, WithRTMP(), WithAPI())

	cfg := srv.Config()
	if cfg == nil {
		t.Fatal("Config() returned nil")
	}
	if !cfg.RTMP.Enabled {
		t.Error("RTMP should be enabled in config")
	}
	if !cfg.API.Enabled {
		t.Error("API should be enabled in config")
	}
	if cfg.RTSP.Enabled {
		t.Error("RTSP should not be enabled in config")
	}
}

func TestStartTestServer_WithAuth(t *testing.T) {
	secret := "test-secret-key"
	srv := StartTestServer(t, WithRTMP(), WithAuth(secret))

	cfg := srv.Config()
	if cfg.Auth.Publish.Mode != "token" {
		t.Errorf("expected publish mode 'token', got %q", cfg.Auth.Publish.Mode)
	}
	if cfg.Auth.Publish.Token.Secret != secret {
		t.Errorf("expected publish token secret %q, got %q", secret, cfg.Auth.Publish.Token.Secret)
	}
	if cfg.Auth.Subscribe.Mode != "token" {
		t.Errorf("expected subscribe mode 'token', got %q", cfg.Auth.Subscribe.Mode)
	}
	if cfg.Auth.Subscribe.Token.Secret != secret {
		t.Errorf("expected subscribe token secret %q, got %q", secret, cfg.Auth.Subscribe.Token.Secret)
	}
}

func TestStartTestServer_UniqueAddresses(t *testing.T) {
	srv1 := StartTestServer(t, WithRTMP(), WithHTTPStream())
	srv2 := StartTestServer(t, WithRTMP(), WithHTTPStream())

	if srv1.RTMPAddr() == srv2.RTMPAddr() {
		t.Errorf("two servers should have different RTMP addresses, both got %s", srv1.RTMPAddr())
	}
	if srv1.HTTPAddr() == srv2.HTTPAddr() {
		t.Errorf("two servers should have different HTTP addresses, both got %s", srv1.HTTPAddr())
	}
}

func TestStartTestServer_ShutdownIdempotent(t *testing.T) {
	srv := StartTestServer(t, WithRTMP())
	// Explicit shutdown before t.Cleanup fires should not panic.
	srv.Shutdown()
}
