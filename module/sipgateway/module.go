package sipgateway

import (
	"context"
	"errors"

	"github.com/im-pingo/liveforge/core"
	sipmod "github.com/im-pingo/liveforge/module/sip"
	"github.com/im-pingo/liveforge/pkg/protocoltest"
	"github.com/prometheus/client_golang/prometheus"
)

// ErrGatewayDisabled is returned by control-plane operations when the module
// was not enabled at startup.
var ErrGatewayDisabled = errors.New("SIP gateway is disabled")

var _ SIPGatewayProvider = (*Module)(nil)

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

	gw, err := NewGateway(cfg, m.sipService, s.StreamHub(), s.GetEventBus())
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

// ListCalls returns active calls for the management API.
func (m *Module) ListCalls() []CallSnapshot {
	if m.gw == nil {
		return []CallSnapshot{}
	}
	return m.gw.ListCalls()
}

// Call returns details for a call-ID.
func (m *Module) Call(callID string) (CallSnapshot, bool) {
	if m.gw == nil {
		return CallSnapshot{}, false
	}
	return m.gw.Call(callID)
}

// Dial initiates an outbound SIP call.
func (m *Module) Dial(ctx context.Context, targetURI, streamKey string) (string, error) {
	if m.gw == nil {
		return "", ErrGatewayDisabled
	}
	return m.gw.Dial(ctx, targetURI, streamKey)
}

// Hangup terminates an active SIP call.
func (m *Module) Hangup(callID string) error {
	if m.gw == nil {
		return ErrGatewayDisabled
	}
	return m.gw.Hangup(callID)
}

// StartLabSession starts a persistent in-process fake SIP device.
func (m *Module) StartLabSession(ctx context.Context, request LabSessionRequest) (LabSessionSnapshot, error) {
	if m.gw == nil {
		return LabSessionSnapshot{}, ErrGatewayDisabled
	}
	return m.gw.StartLabSession(ctx, request)
}

// ListLabSessions returns persistent fake SIP device snapshots.
func (m *Module) ListLabSessions() []LabSessionSnapshot {
	if m.gw == nil {
		return []LabSessionSnapshot{}
	}
	return m.gw.ListLabSessions()
}

// StopLabSession stops a persistent fake SIP device. Repeated stops are safe.
func (m *Module) StopLabSession(id string) error {
	if m.gw == nil {
		return ErrGatewayDisabled
	}
	return m.gw.StopLabSession(id)
}

// Metrics returns bounded-cardinality gateway metrics.
func (m *Module) Metrics() MetricsSnapshot {
	if m.gw == nil {
		return MetricsSnapshot{}
	}
	return m.gw.Metrics()
}

// RunSelfTest runs the local protocol lab. It is safe when the gateway is
// disabled and never contacts a remote SIP endpoint.
func (m *Module) RunSelfTest(ctx context.Context) (protocoltest.Report, error) {
	if m.gw == nil {
		return protocoltest.Report{}, ErrGatewayDisabled
	}
	return m.gw.RunSelfTest(ctx), nil
}

// PrometheusCollectors exposes gateway metrics to the shared metrics module.
func (m *Module) PrometheusCollectors() []prometheus.Collector {
	if m.gw == nil {
		return nil
	}
	return []prometheus.Collector{m.gw}
}
