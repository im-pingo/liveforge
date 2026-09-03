package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResolveUserPathExpandsHomeAndRejectsNamedUsers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	got, err := ResolveUserPath("~/record/{date}/{stream_key}.mp4")
	if err != nil {
		t.Fatalf("ResolveUserPath returned error: %v", err)
	}
	want := filepath.Join(home, "record", "{date}", "{stream_key}.mp4")
	if got != want {
		t.Fatalf("ResolveUserPath = %q, want %q", got, want)
	}
	if _, err := ResolveUserPath("~other/record"); err == nil {
		t.Fatal("ResolveUserPath accepted a named-user tilde path")
	}
}

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
	if cfg.Stream.GOPCacheMaxFrames <= 0 || cfg.Stream.GOPCacheMaxDuration <= 0 || cfg.Stream.GOPCacheMaxBytes <= 0 {
		t.Fatalf("GOP cache bounds must have positive defaults: frames=%d duration=%s bytes=%d", cfg.Stream.GOPCacheMaxFrames, cfg.Stream.GOPCacheMaxDuration, cfg.Stream.GOPCacheMaxBytes)
	}
	if cfg.SIP.Gateway.MaxLabSessions != 16 || cfg.GB28181.MaxLabSessions != 16 {
		t.Fatalf("expected default protocol lab session ceilings of 16, got SIP=%d GB=%d", cfg.SIP.Gateway.MaxLabSessions, cfg.GB28181.MaxLabSessions)
	}
}

func TestLoadConfigGOPCacheBounds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	doc := "stream:\n  gop_cache_max_frames: 7\n  gop_cache_max_duration: 2s\n  gop_cache_max_bytes: 8192\n"
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Stream.GOPCacheMaxFrames != 7 || cfg.Stream.GOPCacheMaxDuration != 2*time.Second || cfg.Stream.GOPCacheMaxBytes != 8192 {
		t.Fatalf("loaded GOP bounds = frames=%d duration=%s bytes=%d", cfg.Stream.GOPCacheMaxFrames, cfg.Stream.GOPCacheMaxDuration, cfg.Stream.GOPCacheMaxBytes)
	}
}

func TestValidateRejectsNonPositiveRingBufferSize(t *testing.T) {
	cfg := Defaults()
	cfg.Stream.RingBufferSize = 0
	if err := Validate(cfg); err == nil || !strings.Contains(err.Error(), "stream.ring_buffer_size") {
		t.Fatalf("Validate() error = %v, want ring buffer size rejection", err)
	}
}

func TestValidateMetricsStreamDetailLimitAllowsZeroAndRejectsNegative(t *testing.T) {
	zero := Defaults()
	zero.Metrics.StreamDetailLimit = 0
	if err := Validate(zero); err != nil {
		t.Fatalf("Validate() rejected metrics.stream_detail_limit=0: %v", err)
	}

	negative := Defaults()
	negative.Metrics.StreamDetailLimit = -1
	if err := Validate(negative); err == nil || !strings.Contains(err.Error(), "metrics.stream_detail_limit must not be negative") {
		t.Fatalf("Validate() error = %v, want negative metrics stream detail limit rejection", err)
	}
}

func TestValidateRejectsInvalidTrustedProxy(t *testing.T) {
	cfg := Defaults()
	cfg.Limits.RateLimit.TrustedProxies = []string{"127.0.0.1", "not-a-network"}
	if err := Validate(cfg); err == nil || !strings.Contains(err.Error(), "limits.rate_limit.trusted_proxies[1]") {
		t.Fatalf("Validate() error = %v, want invalid trusted proxy rejection", err)
	}
}

func TestValidateRejectsUnboundedEnabledGOPCache(t *testing.T) {
	cfg := Defaults()
	cfg.Stream.GOPCache = true
	cfg.Stream.GOPCacheMaxFrames = 0
	cfg.Stream.GOPCacheMaxBytes = 0
	if err := Validate(cfg); err == nil || !strings.Contains(err.Error(), "gop_cache_max_frames or stream.gop_cache_max_bytes") {
		t.Fatalf("Validate() error = %v, want hard GOP bound rejection", err)
	}
}

func TestValidateRecordFormatAndMaxSize(t *testing.T) {
	for _, format := range []string{"flv", "fmp4", "mp4", "ts", "hls", " HLS "} {
		cfg := Defaults()
		cfg.Record.Format = format
		if err := Validate(cfg); err != nil {
			t.Errorf("Validate() rejected record format %q: %v", format, err)
		}
	}

	for _, maxSize := range []string{"", "0", "0MB", "512KB", "1GB", " 256mb "} {
		cfg := Defaults()
		cfg.Record.Segment.MaxSize = maxSize
		if err := Validate(cfg); err != nil {
			t.Errorf("Validate() rejected record.segment.max_size %q: %v", maxSize, err)
		}
	}

	for _, test := range []struct {
		name  string
		field string
		value string
	}{
		{name: "format", field: "record.format", value: "webm"},
		{name: "fractional size", field: "record.segment.max_size", value: "1.5MB"},
		{name: "negative size", field: "record.segment.max_size", value: "-1MB"},
		{name: "unknown suffix", field: "record.segment.max_size", value: "1TB"},
		{name: "overflow size", field: "record.segment.max_size", value: "9223372036854775808B"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := Defaults()
			if test.field == "record.format" {
				cfg.Record.Format = test.value
			} else {
				cfg.Record.Segment.MaxSize = test.value
			}
			if err := Validate(cfg); err == nil || !strings.Contains(err.Error(), test.field) {
				t.Fatalf("Validate() error = %v, want %s rejection", err, test.field)
			}
		})
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

func TestLoadMigratesLegacyManagementBearerToken(t *testing.T) {
	yaml := `
auth:
  api:
    bearer_token: legacy-token
`
	path := filepath.Join(t.TempDir(), "legacy-auth.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.API.Auth.BearerToken != "legacy-token" {
		t.Fatalf("migrated bearer token=%q", cfg.API.Auth.BearerToken)
	}
}

func TestLoadPrefersCurrentManagementBearerTokenPath(t *testing.T) {
	yaml := `
auth:
  api:
    bearer_token: legacy-token
api:
  auth:
    bearer_token: current-token
`
	path := filepath.Join(t.TempDir(), "current-auth.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.API.Auth.BearerToken != "current-token" {
		t.Fatalf("current bearer token was overridden: %q", cfg.API.Auth.BearerToken)
	}
}

func TestLoadRejectsIncompleteConsoleCredentials(t *testing.T) {
	tests := []struct {
		name     string
		username string
		password string
	}{
		{name: "username only", username: "admin"},
		{name: "password only", password: "secret"},
		{name: "blank password", username: "admin", password: "   "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "liveforge.yaml")
			doc := fmt.Sprintf("api:\n  console:\n    username: %q\n    password: %q\n", tt.username, tt.password)
			if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatal("expected incomplete console credentials to be rejected at startup")
			}
		})
	}
}

func TestLoadRejectsLegacyAndNamedTokenCollision(t *testing.T) {
	path := filepath.Join(t.TempDir(), "liveforge.yaml")
	doc := `api:
  auth:
    bearer_token: shared-secret
    tokens:
      - name: readonly
        token: shared-secret
        role: viewer
`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected legacy and named bearer collision to be rejected at startup")
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
    segment_duration: 0.8
    segment_count: 5
    container: "ts"
`
	tmpFile := filepath.Join(t.TempDir(), "test.yaml")
	if err := os.WriteFile(tmpFile, []byte(yaml), 0600); err != nil {
		t.Fatal(err)
	}
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
	if cfg.HTTP.LLHLS.SegmentDuration != 0.8 {
		t.Errorf("segment_duration = %v, want 0.8", cfg.HTTP.LLHLS.SegmentDuration)
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
	if err := os.WriteFile(tmpFile, []byte(yaml), 0600); err != nil {
		t.Fatal(err)
	}
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
	if cfg.HTTP.LLHLS.SegmentDuration != 1.0 {
		t.Errorf("default segment_duration = %v, want 1.0", cfg.HTTP.LLHLS.SegmentDuration)
	}
	if cfg.HTTP.LLHLS.Container != "fmp4" {
		t.Errorf("default container = %q, want %q", cfg.HTTP.LLHLS.Container, "fmp4")
	}
}

func TestTLSConfigured(t *testing.T) {
	tests := []struct {
		name string
		cfg  TLSConfig
		want bool
	}{
		{"empty", TLSConfig{}, false},
		{"cert_only", TLSConfig{CertFile: "cert.pem"}, false},
		{"key_only", TLSConfig{KeyFile: "key.pem"}, false},
		{"both", TLSConfig{CertFile: "cert.pem", KeyFile: "key.pem"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.Configured(); got != tt.want {
				t.Errorf("Configured() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNormalizeMPEGTS(t *testing.T) {
	yaml := `
http_stream:
  llhls:
    container: "mpegts"
`
	tmpFile := filepath.Join(t.TempDir(), "test.yaml")
	if err := os.WriteFile(tmpFile, []byte(yaml), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(tmpFile)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTP.LLHLS.Container != "ts" {
		t.Errorf("expected container 'ts' after normalize, got %q", cfg.HTTP.LLHLS.Container)
	}
}

func TestNormalizeMPEGDash(t *testing.T) {
	yaml := `
http_stream:
  llhls:
    container: "mpeg-ts"
`
	tmpFile := filepath.Join(t.TempDir(), "test.yaml")
	if err := os.WriteFile(tmpFile, []byte(yaml), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(tmpFile)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTP.LLHLS.Container != "ts" {
		t.Errorf("expected container 'ts' after normalize, got %q", cfg.HTTP.LLHLS.Container)
	}
}

func TestGCCConfigDefaults(t *testing.T) {
	yaml := `
webrtc:
  enabled: true
  listen: ":8443"
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
	if !cfg.WebRTC.GCC.Enabled {
		t.Error("expected GCC enabled by default")
	}
	if cfg.WebRTC.GCC.InitialBitrate != 2_000_000 {
		t.Errorf("expected initial bitrate 2000000, got %d", cfg.WebRTC.GCC.InitialBitrate)
	}
	if cfg.WebRTC.GCC.MinBitrate != 100_000 {
		t.Errorf("expected min bitrate 100000, got %d", cfg.WebRTC.GCC.MinBitrate)
	}
	if cfg.WebRTC.GCC.MaxBitrate != 10_000_000 {
		t.Errorf("expected max bitrate 10000000, got %d", cfg.WebRTC.GCC.MaxBitrate)
	}
}

func TestGCCConfigExplicit(t *testing.T) {
	yaml := `
webrtc:
  enabled: true
  gcc:
    enabled: false
    initial_bitrate: 1000000
    min_bitrate: 50000
    max_bitrate: 5000000
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
	if cfg.WebRTC.GCC.Enabled {
		t.Error("expected GCC disabled")
	}
	if cfg.WebRTC.GCC.InitialBitrate != 1_000_000 {
		t.Errorf("expected 1000000, got %d", cfg.WebRTC.GCC.InitialBitrate)
	}
	if cfg.WebRTC.GCC.MinBitrate != 50_000 {
		t.Errorf("expected 50000, got %d", cfg.WebRTC.GCC.MinBitrate)
	}
	if cfg.WebRTC.GCC.MaxBitrate != 5_000_000 {
		t.Errorf("expected 5000000, got %d", cfg.WebRTC.GCC.MaxBitrate)
	}
}

func TestLoadConfigInvalidPath(t *testing.T) {
	_, err := Load("/nonexistent/path/config.yaml")
	if err == nil {
		t.Error("expected error for nonexistent file")
	}
}

func TestLoadConfigInvalidYAML(t *testing.T) {
	tmpFile := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(tmpFile, []byte("{{invalid yaml"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(tmpFile)
	if err == nil {
		t.Error("expected error for invalid YAML")
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

func TestClusterTransportConfigDefaults(t *testing.T) {
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

	if cfg.Cluster.SRT.Latency != 120*time.Millisecond {
		t.Errorf("SRT.Latency = %v, want 120ms", cfg.Cluster.SRT.Latency)
	}
	if cfg.Cluster.SRT.PBKeyLen != 16 {
		t.Errorf("SRT.PBKeyLen = %d, want 16", cfg.Cluster.SRT.PBKeyLen)
	}
	if cfg.Cluster.RTSP.Transport != "tcp" {
		t.Errorf("RTSP.Transport = %q, want tcp", cfg.Cluster.RTSP.Transport)
	}
	if cfg.Cluster.RTP.PortRange != "20000-20100" {
		t.Errorf("RTP.PortRange = %q, want 20000-20100", cfg.Cluster.RTP.PortRange)
	}
	if cfg.Cluster.RTP.SignalingPath != "/api/relay" {
		t.Errorf("RTP.SignalingPath = %q, want /api/relay", cfg.Cluster.RTP.SignalingPath)
	}
	if cfg.Cluster.RTP.RTCPInterval != 5*time.Second {
		t.Errorf("RTP.RTCPInterval = %v, want 5s", cfg.Cluster.RTP.RTCPInterval)
	}
	if cfg.Cluster.RTP.Timeout != 15*time.Second {
		t.Errorf("RTP.Timeout = %v, want 15s", cfg.Cluster.RTP.Timeout)
	}
}
