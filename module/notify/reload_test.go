package notify

import (
	"testing"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
)

func TestModuleOnReloadReplacesHTTPEndpoints(t *testing.T) {
	cfg := config.Defaults()
	cfg.Notify.HTTP.Enabled = true
	cfg.Notify.HTTP.Endpoints = []config.NotifyEndpointConfig{{URL: "http://old.invalid", Retry: 1}}
	server := core.NewServer(cfg)
	m := NewModule()
	if err := m.Init(server); err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	next := *cfg
	next.Notify.HTTP.Endpoints = []config.NotifyEndpointConfig{{URL: "http://new.invalid", Retry: 4}}
	server.UpdateConfig(&next)
	if err := m.OnReload(server); err != nil {
		t.Fatal(err)
	}
	endpoints := m.sender.Endpoints()
	if len(endpoints) != 1 || endpoints[0].URL != "http://new.invalid" || endpoints[0].Retry != 4 {
		t.Fatalf("endpoints = %#v", endpoints)
	}
}

func TestModuleOnReloadStartsHTTPSenderWhenFirstEndpointIsAdded(t *testing.T) {
	initial := config.Defaults()
	initial.Notify.HTTP.Enabled = true
	initial.Notify.HTTP.Endpoints = nil
	server := core.NewServer(initial)
	m := NewModule()
	if err := m.Init(server); err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if m.sender != nil {
		t.Fatal("sender should not start without endpoints")
	}

	next := *initial
	next.Notify.HTTP.Endpoints = []config.NotifyEndpointConfig{{URL: "http://127.0.0.1:1/hook", Retry: 1}}
	server.UpdateConfig(&next)
	if err := m.OnReload(server); err != nil {
		t.Fatal(err)
	}
	if m.sender == nil {
		t.Fatal("sender was not started for the first hot-added endpoint")
	}
}
