package sipgateway

import (
	"github.com/im-pingo/liveforge/core"
	sipmod "github.com/im-pingo/liveforge/module/sip"
)

// Module implements core.Module for the SIP-to-stream gateway.
type Module struct {
	gw         *Gateway
	sipService sipmod.SIPService
}

// NewModule creates a new SIP gateway module.
func NewModule(sipService sipmod.SIPService) *Module {
	return &Module{sipService: sipService}
}

// Name returns the module name.
func (m *Module) Name() string { return "sipgateway" }

// Init initializes the gateway if enabled in config.
func (m *Module) Init(s *core.Server) error {
	cfg := s.Config().SIP.Gateway
	if !cfg.Enabled {
		return nil
	}

	gw, err := NewGateway(cfg, m.sipService, s.StreamHub(), s.GetEventBus(), s)
	if err != nil {
		return err
	}
	m.gw = gw
	return nil
}

// Hooks returns empty hooks — gateway uses SIP event dispatch.
func (m *Module) Hooks() []core.HookRegistration { return nil }

// Close stops the gateway.
func (m *Module) Close() error {
	if m.gw != nil {
		m.gw.Close()
	}
	return nil
}

// Gateway returns the gateway instance, or nil if disabled.
func (m *Module) Gateway() *Gateway {
	return m.gw
}
