package auth

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
)

// Module owns the single runtime authorizer installed on the server.
type Module struct {
	server *core.Server
}

// NewModule creates a new auth module.
func NewModule() *Module {
	return &Module{}
}

// Name returns the module name.
func (m *Module) Name() string { return "auth" }

func (m *Module) Init(server *core.Server) error {
	m.server = server
	server.SetAuthorizer(m)
	cfg := server.Config().Auth
	slog.Info("initialized", "module", "auth", "enabled", cfg.Enabled,
		"publish_mode", cfg.Publish.Mode, "subscribe_mode", cfg.Subscribe.Mode)
	return nil
}

// Hooks is empty because publish/subscribe events describe committed state and
// must not be used as authorization callbacks.
func (m *Module) Hooks() []core.HookRegistration { return nil }

func (m *Module) Close() error {
	if m.server != nil {
		m.server.SetAuthorizer(nil)
	}
	return nil
}

func (m *Module) Authorize(_ context.Context, request core.AuthorizationRequest) error {
	if m.server == nil {
		return fmt.Errorf("auth module is not initialized")
	}
	cfg := m.server.RuntimeConfig().Auth()
	if !cfg.Enabled {
		return nil
	}

	rule, action, err := authorizationRule(cfg, request.Action)
	if err != nil {
		return err
	}
	stage := rule.Stage
	if stage == "" {
		stage = string(core.AuthorizationPostConnect)
	}
	if string(request.Stage) != stage {
		return nil
	}

	eventContext := &core.EventContext{
		StreamKey: request.StreamKey, Protocol: request.Protocol,
		RemoteAddr: request.RemoteAddr, Params: request.Params, Extra: request.Extra,
	}
	if err := checkAuth(rule, eventContext, action); err != nil {
		slog.Warn("authorization rejected", "module", "auth", "action", action,
			"stage", request.Stage, "stream", request.StreamKey,
			"protocol", request.Protocol, "remote", request.RemoteAddr, "error", err)
		return err
	}
	return nil
}

func authorizationRule(cfg config.AuthConfig, action core.AuthorizationAction) (config.AuthRuleConfig, string, error) {
	switch action {
	case core.AuthorizationPublish:
		return cfg.Publish, string(core.AuthorizationPublish), nil
	case core.AuthorizationSubscribe:
		return cfg.Subscribe, string(core.AuthorizationSubscribe), nil
	default:
		return config.AuthRuleConfig{}, "", fmt.Errorf("unknown authorization action %q", action)
	}
}
