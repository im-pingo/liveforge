package auth

import (
	"log"

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
	log.Printf("[auth] enabled, publish mode: %s, subscribe mode: %s", m.cfg.Publish.Mode, m.cfg.Subscribe.Mode)
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
		log.Printf("[auth] publish rejected for %s from %s: %v", ctx.StreamKey, ctx.RemoteAddr, err)
		return err
	}
	return nil
}

func (m *Module) onSubscribe(ctx *core.EventContext) error {
	if err := checkAuth(m.cfg.Subscribe, ctx, "subscribe"); err != nil {
		log.Printf("[auth] subscribe rejected for %s from %s: %v", ctx.StreamKey, ctx.RemoteAddr, err)
		return err
	}
	return nil
}
