package runtime

import "testing"

import "github.com/im-pingo/liveforge/config"

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
		"record.path", "dvr.window", "api.auth.tokens", "api.console.role",
		"http_stream.hls.segment_duration", "http_stream.dash.playlist_size", "http_stream.llhls.container",
		"webrtc.gcc.max_bitrate",
	} {
		if got := classifyPath(path); got != ChangeHot {
			t.Errorf("classifyPath(%q)=%q want hot_reload", path, got)
		}
	}
	for _, path := range []string{
		"notify.websocket.path", "notify.alive_interval", "api.listen", "api.tls", "api.audit.max_entries",
		"http_stream.cors", "webrtc.ice_servers", "metrics.path", "runtime.poll_interval",
	} {
		if got := classifyPath(path); got != ChangeRestart {
			t.Errorf("classifyPath(%q)=%q want restart_required", path, got)
		}
	}
}
