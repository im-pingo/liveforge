package runtime

import (
	"testing"

	"github.com/im-pingo/liveforge/config"
)

func TestBuildSourceSelectsHTTPAndFallsBackToFile(t *testing.T) {
	httpCfg := config.RuntimeConfig{Source: "http", HTTP: config.RuntimeHTTPSourceConfig{URL: "http://127.0.0.1:8080/config"}}
	source, err := BuildSource(httpCfg, "configs/liveforge.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if source.Name() != "http" {
		t.Fatalf("source = %q", source.Name())
	}
	_ = source.Close()

	fileSource, err := BuildSource(config.RuntimeConfig{}, "configs/liveforge.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if fileSource.Name() != "file" {
		t.Fatalf("fallback source = %q", fileSource.Name())
	}
	_ = fileSource.Close()
}

func TestBuildSourceRejectsMissingRemoteAddress(t *testing.T) {
	_, err := BuildSource(config.RuntimeConfig{Source: "consul", Consul: config.RuntimeConsulSourceConfig{Prefix: "liveforge"}}, "")
	if err == nil {
		t.Fatal("expected missing consul address error")
	}
}
