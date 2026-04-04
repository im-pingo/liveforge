package cluster

import (
	"log/slog"

	"github.com/im-pingo/liveforge/core"
)

// Module implements core.Module for cluster forwarding and origin pull.
type Module struct {
	forward  *ForwardManager
	origin   *OriginManager
	registry *TransportRegistry
}

// NewModule creates a new cluster module.
func NewModule() *Module {
	return &Module{}
}

// Name returns the module name.
func (m *Module) Name() string { return "cluster" }

// Init initializes the cluster module based on configuration.
func (m *Module) Init(s *core.Server) error {
	cfg := s.Config().Cluster
	hub := s.StreamHub()
	bus := s.GetEventBus()

	m.registry = NewTransportRegistry()
	m.registry.Register(NewRTMPTransport())
	m.registry.Register(NewSRTTransport(cfg.SRT))
	m.registry.Register(NewRTSPTransport(cfg.RTSP))
	m.registry.Register(NewRTPTransport(cfg.RTP, s))
	m.registry.Register(NewGBTransport(cfg.GB28181, s))

	if cfg.Forward.Enabled && (len(cfg.Forward.Targets) > 0 || cfg.Forward.ScheduleURL != "") {
		fwdScheduler := NewScheduler(
			cfg.Forward.ScheduleURL,
			cfg.Forward.Targets,
			cfg.Forward.SchedulePriority,
			cfg.Forward.ScheduleTimeout,
		)
		m.forward = NewForwardManager(
			hub, bus,
			fwdScheduler,
			m.registry,
			cfg.Forward.RetryMax,
			cfg.Forward.RetryInterval,
		)
		slog.Info("cluster forward enabled", "module", "cluster",
			"static_targets", len(cfg.Forward.Targets),
			"schedule_url", cfg.Forward.ScheduleURL)
	}

	if cfg.Origin.Enabled && (len(cfg.Origin.Servers) > 0 || cfg.Origin.ScheduleURL != "") {
		origScheduler := NewScheduler(
			cfg.Origin.ScheduleURL,
			cfg.Origin.Servers,
			cfg.Origin.SchedulePriority,
			cfg.Origin.ScheduleTimeout,
		)
		m.origin = NewOriginManager(
			hub, bus,
			origScheduler,
			m.registry,
			cfg.Origin.RetryMax,
			cfg.Origin.RetryDelay,
			cfg.Origin.IdleTimeout,
		)
		slog.Info("cluster origin pull enabled", "module", "cluster",
			"static_servers", len(cfg.Origin.Servers),
			"schedule_url", cfg.Origin.ScheduleURL)
	}

	return nil
}

// Hooks returns event hooks for both forward and origin managers.
func (m *Module) Hooks() []core.HookRegistration {
	var hooks []core.HookRegistration
	if m.forward != nil {
		hooks = append(hooks, m.forward.Hooks()...)
	}
	if m.origin != nil {
		hooks = append(hooks, m.origin.Hooks()...)
	}
	return hooks
}

// Close shuts down both forward and origin managers.
func (m *Module) Close() error {
	if m.forward != nil {
		m.forward.Close()
	}
	if m.origin != nil {
		m.origin.Close()
	}
	if m.registry != nil {
		m.registry.Close()
	}
	return nil
}

// ForwardManager returns the forward manager, if enabled.
func (m *Module) ForwardManager() *ForwardManager {
	return m.forward
}

// OriginManager returns the origin manager, if enabled.
func (m *Module) OriginManager() *OriginManager {
	return m.origin
}
