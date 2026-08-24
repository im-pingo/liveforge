package cluster

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/im-pingo/liveforge/core"
	"github.com/prometheus/client_golang/prometheus"
)

var errPeerAdminCredentialUnavailable = errors.New("management authentication is configured but no admin credential is available")

func authorizePeerRequest(req *http.Request, server *core.Server) error {
	if server == nil {
		return nil
	}
	auth := server.Config().API.Auth
	if auth.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+auth.BearerToken)
		return nil
	}
	if len(auth.Tokens) == 0 {
		return nil
	}
	for _, token := range auth.Tokens {
		if strings.EqualFold(strings.TrimSpace(token.Role), "admin") && token.Token != "" {
			req.Header.Set("Authorization", "Bearer "+token.Token)
			return nil
		}
	}
	return errPeerAdminCredentialUnavailable
}

// Module implements core.Module for cluster forwarding and origin pull.
type Module struct {
	forward   *ForwardManager
	origin    *OriginManager
	health    *HealthTracker
	relayPool *RelayPool
	registry  *TransportRegistry
	metrics   *RelayMetrics
	close     sync.Once
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
	m.metrics = newRelayMetrics()
	rtpTransport := NewRTPTransport(cfg.RTP, s)
	rtpTransport.metrics = m.metrics
	m.registry.Register(rtpTransport)
	m.registry.Register(NewGBTransport(cfg.GB28181, s))

	if cfg.HealthCheck.Enabled {
		m.health = NewHealthTracker(cfg.HealthCheck)
		slog.Info("cluster health check enabled", "module", "cluster",
			"evict_threshold", cfg.HealthCheck.EvictThreshold,
			"interval", cfg.HealthCheck.Interval)
	}

	if cfg.RelayPool.MaxPerHost > 0 {
		m.relayPool = NewRelayPool(cfg.RelayPool.MaxPerHost)
		slog.Info("cluster relay pool enabled", "module", "cluster",
			"max_per_host", cfg.RelayPool.MaxPerHost)
	}

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
			m.health,
			m.relayPool,
			cfg.Forward.RetryMax,
			cfg.Forward.RetryInterval,
			m.metrics,
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
			m.health,
			m.relayPool,
			cfg.Origin.RetryMax,
			cfg.Origin.RetryDelay,
			cfg.Origin.IdleTimeout,
			m.metrics,
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

// OnReload updates scheduling, retry, idle, and health policies for new relay
// attempts. Transport settings, port ranges, pool capacity, and module
// enablement remain restart-required. Peer management credentials are resolved
// from the server config for each signaling request and rotate independently.
func (m *Module) OnReload(s *core.Server) error {
	cfg := s.Config().Cluster
	if m.health != nil {
		m.health.UpdateConfig(cfg.HealthCheck)
	}
	if m.forward != nil {
		m.forward.UpdatePolicy(cfg.Forward, m.health)
	}
	if m.origin != nil {
		m.origin.UpdatePolicy(cfg.Origin, m.health)
	}
	return nil
}

// Close shuts down both forward and origin managers.
func (m *Module) Close() error {
	m.close.Do(func() {
		if m.forward != nil {
			m.forward.Close()
		}
		if m.origin != nil {
			m.origin.Close()
		}
		if m.health != nil {
			m.health.Close()
		}
		if m.registry != nil {
			m.registry.Close()
		}
	})
	return nil
}

// RelayMetrics returns the module's relay collector, if initialized.
func (m *Module) RelayMetrics() *RelayMetrics { return m.metrics }

// PrometheusCollectors exposes cluster collectors to the metrics module
// without registering them globally.
func (m *Module) PrometheusCollectors() []prometheus.Collector {
	if m.metrics == nil {
		return nil
	}
	return []prometheus.Collector{m.metrics}
}

// ForwardManager returns the forward manager, if enabled.
func (m *Module) ForwardManager() *ForwardManager {
	return m.forward
}

// OriginManager returns the origin manager, if enabled.
func (m *Module) OriginManager() *OriginManager {
	return m.origin
}
