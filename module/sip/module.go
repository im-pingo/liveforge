package sip

import (
	"github.com/im-pingo/liveforge/core"
)

// Module implements core.Module for SIP transport.
type Module struct {
	svc *service
}

// NewModule creates a new SIP module.
func NewModule() *Module {
	return &Module{
		svc: newService(),
	}
}

// Name returns the module name.
func (m *Module) Name() string { return "sip" }

// Init creates the sipgo UA, starts listeners, and sets up method dispatch.
func (m *Module) Init(s *core.Server) error {
	return m.svc.init(s.Config().SIP)
}

// Hooks returns empty hooks — SIP is pure transport, no lifecycle hooks.
func (m *Module) Hooks() []core.HookRegistration { return nil }

// Close gracefully stops listeners and drains in-flight transactions.
func (m *Module) Close() error {
	m.svc.close()
	return nil
}

// Service returns the SIPService implementation for injection into dependent modules.
func (m *Module) Service() SIPService {
	return m.svc
}
