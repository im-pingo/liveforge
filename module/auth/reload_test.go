package auth

import (
	"testing"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
)

func TestModuleOnReloadChangesPublishPolicy(t *testing.T) {
	cfg := config.Defaults()
	cfg.Auth.Publish.Mode = "none"
	server := core.NewServer(cfg)
	m := NewModule()
	if err := m.Init(server); err != nil {
		t.Fatal(err)
	}
	if err := m.onPublish(&core.EventContext{StreamKey: "live/test"}); err != nil {
		t.Fatalf("initial policy rejected: %v", err)
	}

	next := *cfg
	next.Auth.Publish.Mode = "token"
	next.Auth.Publish.Token.Secret = "rotated-secret"
	server.UpdateConfig(&next)
	if err := m.OnReload(server); err != nil {
		t.Fatal(err)
	}
	if err := m.onPublish(&core.EventContext{StreamKey: "live/test"}); err == nil {
		t.Fatal("reloaded token policy accepted a request without a token")
	}
}
