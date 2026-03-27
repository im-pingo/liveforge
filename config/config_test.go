package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	yaml := `
server:
  name: "test-server"
  log_level: debug
  drain_timeout: 10s

rtmp:
  enabled: true
  listen: ":1935"
  chunk_size: 4096

stream:
  gop_cache: true
  gop_cache_num: 1
  audio_cache_ms: 1000
  ring_buffer_size: 512
  idle_timeout: 30s
  no_publisher_timeout: 15s
`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if cfg.Server.Name != "test-server" {
		t.Errorf("expected name test-server, got %s", cfg.Server.Name)
	}
	if cfg.Server.LogLevel != "debug" {
		t.Errorf("expected log_level debug, got %s", cfg.Server.LogLevel)
	}
	if !cfg.RTMP.Enabled {
		t.Error("expected RTMP enabled")
	}
	if cfg.RTMP.Listen != ":1935" {
		t.Errorf("expected RTMP listen :1935, got %s", cfg.RTMP.Listen)
	}
	if cfg.Stream.RingBufferSize != 512 {
		t.Errorf("expected ring_buffer_size 512, got %d", cfg.Stream.RingBufferSize)
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	yaml := `{}`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if cfg.RTMP.Listen != ":1935" {
		t.Errorf("expected default RTMP listen :1935, got %s", cfg.RTMP.Listen)
	}
	if cfg.Stream.RingBufferSize != 1024 {
		t.Errorf("expected default ring_buffer_size 1024, got %d", cfg.Stream.RingBufferSize)
	}
}

func TestLoadConfigEnvExpansion(t *testing.T) {
	t.Setenv("TEST_JWT_SECRET", "mysecret123")
	yaml := `
auth:
  enabled: true
  publish:
    mode: "token"
    token:
      secret: "${TEST_JWT_SECRET}"
`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if cfg.Auth.Publish.Token.Secret != "mysecret123" {
		t.Errorf("expected expanded secret mysecret123, got %s", cfg.Auth.Publish.Token.Secret)
	}
}

func TestLoadConfigSRT(t *testing.T) {
	yaml := `
srt:
  enabled: true
  listen: ":6001"
  latency: 200
  passphrase: "mysecretpass"
  pbkeylen: 16
`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if !cfg.SRT.Enabled {
		t.Error("expected SRT enabled")
	}
	if cfg.SRT.Listen != ":6001" {
		t.Errorf("expected SRT listen :6001, got %s", cfg.SRT.Listen)
	}
	if cfg.SRT.Latency != 200 {
		t.Errorf("expected SRT latency 200, got %d", cfg.SRT.Latency)
	}
	if cfg.SRT.Passphrase != "mysecretpass" {
		t.Errorf("expected SRT passphrase mysecretpass, got %s", cfg.SRT.Passphrase)
	}
	if cfg.SRT.PBKeyLen != 16 {
		t.Errorf("expected SRT pbkeylen 16, got %d", cfg.SRT.PBKeyLen)
	}
}

func TestLLHLSConfigParsing(t *testing.T) {
	yaml := `
http_stream:
  llhls:
    enabled: true
    part_duration: 0.3
    segment_count: 5
    container: "ts"
`
	tmpFile := filepath.Join(t.TempDir(), "test.yaml")
	os.WriteFile(tmpFile, []byte(yaml), 0644)
	cfg, err := Load(tmpFile)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.HTTP.LLHLS.Enabled {
		t.Error("expected llhls.enabled=true")
	}
	if cfg.HTTP.LLHLS.PartDuration != 0.3 {
		t.Errorf("part_duration = %v, want 0.3", cfg.HTTP.LLHLS.PartDuration)
	}
	if cfg.HTTP.LLHLS.SegmentCount != 5 {
		t.Errorf("segment_count = %v, want 5", cfg.HTTP.LLHLS.SegmentCount)
	}
	if cfg.HTTP.LLHLS.Container != "ts" {
		t.Errorf("container = %q, want %q", cfg.HTTP.LLHLS.Container, "ts")
	}
}

func TestLLHLSConfigDefaults(t *testing.T) {
	yaml := `
http_stream:
  listen: ":8080"
`
	tmpFile := filepath.Join(t.TempDir(), "test.yaml")
	os.WriteFile(tmpFile, []byte(yaml), 0644)
	cfg, err := Load(tmpFile)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTP.LLHLS.Enabled {
		t.Error("expected llhls.enabled=false by default")
	}
	if cfg.HTTP.LLHLS.PartDuration != 0.2 {
		t.Errorf("default part_duration = %v, want 0.2", cfg.HTTP.LLHLS.PartDuration)
	}
	if cfg.HTTP.LLHLS.Container != "fmp4" {
		t.Errorf("default container = %q, want %q", cfg.HTTP.LLHLS.Container, "fmp4")
	}
}

func TestLoadConfigSRTDefaults(t *testing.T) {
	yaml := `{}`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if cfg.SRT.Listen != ":6000" {
		t.Errorf("expected default SRT listen :6000, got %s", cfg.SRT.Listen)
	}
	if cfg.SRT.Latency != 120 {
		t.Errorf("expected default SRT latency 120, got %d", cfg.SRT.Latency)
	}
}
