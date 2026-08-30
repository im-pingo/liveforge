package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/im-pingo/liveforge/config"
)

func TestParseDocumentRejectsRemovedStreamSetting(t *testing.T) {
	_, err := ParseDocument([]byte("stream:\n  audio_cache_ms: 1000\n"))
	const want = "stream.audio_cache_ms has been removed; audio is interleaved in the GOP cache"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("removed setting error = %v, want %q", err, want)
	}
}

func TestParseDocumentRejectsEnabledLLHLSZeroSegmentDuration(t *testing.T) {
	_, err := ParseDocument([]byte("http_stream:\n  llhls:\n    enabled: true\n    segment_duration: 0\n"))
	if err == nil || !strings.Contains(err.Error(), "http_stream.llhls.segment_duration must be greater than zero") {
		t.Fatalf("zero LL-HLS segment duration error = %v", err)
	}
}

func TestParseDocumentRejectsRemovedStreamSettingThroughYAMLIndirection(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "testdata", "removed-settings", "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("no removed-setting fixtures found")
	}

	for _, path := range paths {
		path := path
		t.Run(strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			_, err = ParseDocument(data)
			const want = "stream.audio_cache_ms has been removed; audio is interleaved in the GOP cache"
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("removed setting error = %v, want %q", err, want)
			}
		})
	}
}

func TestParseDocumentExpandsEnvironmentLikeBootstrapLoad(t *testing.T) {
	t.Setenv("LIVEFORGE_TEST_API_TOKEN", "runtime-admin-secret")
	t.Setenv("LIVEFORGE_TEST_PUBLISH_SECRET", "runtime-publish-secret")

	cfg, err := ParseDocument([]byte(`api:
  auth:
    bearer_token: "${LIVEFORGE_TEST_API_TOKEN}"
auth:
  publish:
    token:
      secret: "${LIVEFORGE_TEST_PUBLISH_SECRET}"
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.API.Auth.BearerToken != "runtime-admin-secret" {
		t.Fatalf("runtime bearer token=%q, want expanded value", cfg.API.Auth.BearerToken)
	}
	if cfg.Auth.Publish.Token.Secret != "runtime-publish-secret" {
		t.Fatalf("runtime publish secret=%q, want expanded value", cfg.Auth.Publish.Token.Secret)
	}
}

func TestValidateKnownDocumentRejectsSecondYAMLDocument(t *testing.T) {
	_, err := ValidateKnownDocument([]byte("server:\n  name: liveforge\n---\nmalicious_or_unknown:\n  value: ignored\n"))
	if err == nil {
		t.Fatal("expected second YAML document to be rejected")
	}
}

func TestParseDocumentAppliesDefaultsAndNormalizesContainer(t *testing.T) {
	cfg, err := ParseDocument([]byte("http_stream:\n  llhls:\n    container: mpeg-ts\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTP.Listen != ":8080" {
		t.Fatalf("default listen = %q", cfg.HTTP.Listen)
	}
	if cfg.HTTP.LLHLS.Container != "ts" {
		t.Fatalf("container = %q", cfg.HTTP.LLHLS.Container)
	}
}

func TestParseDocumentRejectsInvalidRuntimeValues(t *testing.T) {
	_, err := ParseDocument([]byte("webrtc:\n  udp_port_range: [30000, 20000]\n"))
	if err == nil {
		t.Fatal("expected invalid port range error")
	}
}

func TestParseDocumentRejectsInvalidAPIRoleAndDuplicateToken(t *testing.T) {
	_, err := ParseDocument([]byte("api:\n  auth:\n    tokens:\n      - name: first\n        token: same\n        role: viewer\n      - name: second\n        token: same\n        role: owner\n"))
	if err == nil {
		t.Fatal("expected invalid role or duplicate token error")
	}
}

func TestParseDocumentRejectsIncompleteOrBlankConsoleCredentials(t *testing.T) {
	tests := []struct {
		name string
		doc  string
	}{
		{name: "username only", doc: "api:\n  console:\n    username: admin\n    password: \"\"\n"},
		{name: "password only", doc: "api:\n  console:\n    username: \"\"\n    password: secret\n"},
		{name: "blank username", doc: "api:\n  console:\n    username: \"   \"\n    password: secret\n"},
		{name: "blank password", doc: "api:\n  console:\n    username: admin\n    password: \"   \"\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParseDocument([]byte(tt.doc)); err == nil {
				t.Fatal("expected incomplete console credentials to be rejected")
			}
		})
	}
}

func TestParseDocumentRejectsLegacyAndNamedTokenCollision(t *testing.T) {
	_, err := ParseDocument([]byte(`api:
  auth:
    bearer_token: shared-secret
    tokens:
      - name: readonly
        token: shared-secret
        role: viewer
`))
	if err == nil {
		t.Fatal("expected legacy and named bearer collision to be rejected")
	}
}

func TestDiffClassifiesHotRestartAndImmutablePaths(t *testing.T) {
	oldCfg := config.Defaults()
	newCfg := config.Defaults()
	newCfg.Server.Name = "new-identity"
	newCfg.Limits.MaxConnections = 12
	newCfg.RTMP.Listen = ":2935"
	changes, err := diffConfigs(oldCfg, newCfg)
	if err != nil {
		t.Fatal(err)
	}
	classes := make(map[string]ChangeClass, len(changes))
	for _, change := range changes {
		classes[change.Path] = change.Class
	}
	if classes["server.name"] != ChangeImmutable {
		t.Fatalf("server.name class = %q", classes["server.name"])
	}
	if classes["limits.max_connections"] != ChangeHot {
		t.Fatalf("limits class = %q", classes["limits.max_connections"])
	}
	if classes["rtmp.listen"] != ChangeRestart {
		t.Fatalf("rtmp class = %q", classes["rtmp.listen"])
	}
}

func TestClassifyModuleEnablementAndStructuralGCCAsRestartRequired(t *testing.T) {
	paths := []string{
		"auth.enabled", "notify.http.enabled", "notify.websocket.enabled",
		"record.enabled", "dvr.enabled", "dvr.listen", "cluster.forward.enabled",
		"cluster.origin.enabled", "webrtc.gcc.enabled", "stream.ring_buffer_size",
	}
	for _, path := range paths {
		if got := classifyPath(path); got != ChangeRestart {
			t.Errorf("classifyPath(%q) = %q, want restart_required", path, got)
		}
	}
	if got := classifyPath("auth.publish.token.secret"); got != ChangeHot {
		t.Errorf("auth token class = %q, want hot", got)
	}
}

func TestClassifyClusterPolicyAndTransportPaths(t *testing.T) {
	for _, path := range []string{"cluster.forward.targets", "cluster.forward.retry_max", "cluster.origin.servers", "cluster.health_check.interval"} {
		if got := classifyPath(path); got != ChangeHot {
			t.Errorf("classifyPath(%q) = %q, want hot", path, got)
		}
	}
	for _, path := range []string{"cluster.relay_pool.max_per_host", "cluster.srt.passphrase", "cluster.rtp.port_range", "cluster.rtsp.transport"} {
		if got := classifyPath(path); got != ChangeRestart {
			t.Errorf("classifyPath(%q) = %q, want restart", path, got)
		}
	}
}

func TestClassifyOnlyImplementedRuntimePoliciesAsHot(t *testing.T) {
	for _, path := range []string{
		"server.log_level", "server.drain_timeout", "limits.max_streams", "limits.rate_limit.rate",
		"stream.gop_cache_num", "auth.subscribe.callback.url", "notify.http.endpoints",
		"dvr.window", "api.auth.tokens", "api.console.role",
		"http_stream.hls.segment_duration", "http_stream.dash.playlist_size", "http_stream.llhls.container",
		"http_stream.llhls.segment_duration",
		"webrtc.gcc.max_bitrate",
	} {
		if got := classifyPath(path); got != ChangeHot {
			t.Errorf("classifyPath(%q)=%q want hot_reload", path, got)
		}
	}
	for _, path := range []string{
		"notify.websocket.path", "notify.alive_interval", "api.listen", "api.tls", "api.audit.max_entries",
		"http_stream.cors", "webrtc.ice_servers", "metrics.path", "runtime.poll_interval", "record.path", "dvr.path",
	} {
		if got := classifyPath(path); got != ChangeRestart {
			t.Errorf("classifyPath(%q)=%q want restart_required", path, got)
		}
	}
}
