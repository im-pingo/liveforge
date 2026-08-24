package auth

import (
	"log/slog"
	"sync"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
)

// Module implements authentication for publish and subscribe events.
type Module struct {
	mu  sync.RWMutex
	cfg config.AuthConfig
}

// NewModule creates a new auth module.
func NewModule() *Module {
	return &Module{}
}

// Name returns the module name.
func (m *Module) Name() string { return "auth" }

// Init reads auth config from the server.
func (m *Module) Init(s *core.Server) error {
	m.mu.Lock()
	m.cfg = s.Config().Auth
	m.mu.Unlock()
	slog.Info("enabled", "module", "auth", "publish_mode", m.cfg.Publish.Mode, "subscribe_mode", m.cfg.Subscribe.Mode)
	return nil
}

// OnReload atomically publishes new authentication rules. In-flight checks
// finish against their captured rule while subsequent checks use the new one.
func (m *Module) OnReload(s *core.Server) error {
	m.mu.Lock()
	m.cfg = s.Config().Auth
	m.mu.Unlock()
	return nil
}

// Hooks returns sync hooks for EventPublish and EventSubscribe at priority 10.
func (m *Module) Hooks() []core.HookRegistration {
	return []core.HookRegistration{
		{
			Event:    core.EventPublish,
			Mode:     core.HookSync,
			Priority: 10,
			Handler:  m.onPublish,
		},
		{
			Event:    core.EventSubscribe,
			Mode:     core.HookSync,
			Priority: 10,
			Handler:  m.onSubscribe,
		},
	}
}

// Close is a no-op for the auth module.
func (m *Module) Close() error { return nil }

func (m *Module) onPublish(ctx *core.EventContext) error {
	m.mu.RLock()
	rule := m.cfg.Publish
	m.mu.RUnlock()
	if err := checkAuth(rule, ctx, "publish"); err != nil {
		slog.Warn("publish rejected", "module", "auth", "stream", ctx.StreamKey, "remote", ctx.RemoteAddr, "error", err)
		return err
	}
	return nil
}

func (m *Module) onSubscribe(ctx *core.EventContext) error {
	m.mu.RLock()
	rule := m.cfg.Subscribe
	m.mu.RUnlock()
	if err := checkAuth(rule, ctx, "subscribe"); err != nil {
		slog.Warn("subscribe rejected", "module", "auth", "stream", ctx.StreamKey, "remote", ctx.RemoteAddr, "error", err)
		return err
	}
	return nil
}
