package core

import (
	"testing"

	"github.com/im-pingo/liveforge/config"
)

type mockModule struct {
	name   string
	inited bool
	closed bool
	hooks  []HookRegistration
}

func (m *mockModule) Name() string             { return m.name }
func (m *mockModule) Init(s *Server) error      { m.inited = true; return nil }
func (m *mockModule) Hooks() []HookRegistration { return m.hooks }
func (m *mockModule) Close() error              { m.closed = true; return nil }

func TestServerModuleLifecycle(t *testing.T) {
	cfg := &config.Config{}
	cfg.Server.Name = "test"
	s := NewServer(cfg)

	mod := &mockModule{name: "test-module"}
	s.RegisterModule(mod)

	if err := s.Init(); err != nil {
		t.Fatalf("Init error: %v", err)
	}
	if !mod.inited {
		t.Error("expected module to be inited")
	}

	s.Shutdown()
	if !mod.closed {
		t.Error("expected module to be closed")
	}
}

func TestServerModuleCloseReverseOrder(t *testing.T) {
	cfg := &config.Config{}
	s := NewServer(cfg)

	var order []string
	s.RegisterModule(&orderTrackModule{name: "first", order: &order})
	s.RegisterModule(&orderTrackModule{name: "second", order: &order})

	_ = s.Init()
	s.Shutdown()

	if len(order) != 2 || order[0] != "second" || order[1] != "first" {
		t.Errorf("expected close order [second, first], got %v", order)
	}
}

type orderTrackModule struct {
	name  string
	order *[]string
}

func (m *orderTrackModule) Name() string             { return m.name }
func (m *orderTrackModule) Init(s *Server) error      { return nil }
func (m *orderTrackModule) Hooks() []HookRegistration { return nil }
func (m *orderTrackModule) Close() error              { *m.order = append(*m.order, m.name); return nil }

func TestServerStreamHub(t *testing.T) {
	cfg := &config.Config{}
	cfg.Stream.RingBufferSize = 256
	s := NewServer(cfg)
	if s.StreamHub() == nil {
		t.Fatal("expected StreamHub to be initialized")
	}
}
