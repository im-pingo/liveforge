package auth

import (
	"log/slog"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
)

// Module implements authentication for publish and subscribe events.
type Module struct {
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
	m.cfg = s.Config().Auth
	slog.Info("enabled", "module", "auth", "publish_mode", m.cfg.Publish.Mode, "subscribe_mode", m.cfg.Subscribe.Mode)
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
	if err := checkAuth(m.cfg.Publish, ctx, "publish"); err != nil {
		slog.Warn("publish rejected", "module", "auth", "stream", ctx.StreamKey, "remote", ctx.RemoteAddr, "error", err)
		return err
	}
	return nil
}

func (m *Module) onSubscribe(ctx *core.EventContext) error {
	if err := checkAuth(m.cfg.Subscribe, ctx, "subscribe"); err != nil {
		slog.Warn("subscribe rejected", "module", "auth", "stream", ctx.StreamKey, "remote", ctx.RemoteAddr, "error", err)
		return err
	}
	return nil
}
