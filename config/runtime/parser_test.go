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
