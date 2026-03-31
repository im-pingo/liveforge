package metrics

import (
	"log/slog"
	"net"
	"net/http"
	"sync"

	"github.com/im-pingo/liveforge/core"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Module exposes Prometheus metrics via an HTTP endpoint.
type Module struct {
	listener  net.Listener
	httpSrv   *http.Server
	wg        sync.WaitGroup
	collector *Collector
}

// NewModule creates a new metrics module.
func NewModule() *Module {
	return &Module{}
}

// Name returns the module name.
func (m *Module) Name() string { return "metrics" }

// Init starts the metrics HTTP server and registers the custom collector.
func (m *Module) Init(s *core.Server) error {
	cfg := s.Config().Metrics

	m.collector = NewCollector(s)

	registry := prometheus.NewRegistry()
	registry.MustRegister(m.collector)
	registry.MustRegister(collectors.NewGoCollector())
	registry.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	path := cfg.Path
	if path == "" {
		path = "/metrics"
	}

	mux := http.NewServeMux()
	mux.Handle(path, promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	m.listener = ln

	m.httpSrv = &http.Server{Handler: mux}

	slog.Info("listening", "module", "metrics", "addr", ln.Addr(), "path", path)

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		if err := m.httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Error("serve error", "module", "metrics", "error", err)
		}
	}()

	return nil
}

// Hooks returns the module's event hooks (none for metrics).
func (m *Module) Hooks() []core.HookRegistration { return nil }

// Close shuts down the metrics server.
func (m *Module) Close() error {
	if m.httpSrv != nil {
		m.httpSrv.Close()
	}
	m.wg.Wait()
	slog.Info("stopped", "module", "metrics")
	return nil
}

// Addr returns the listener address (useful for tests).
func (m *Module) Addr() net.Addr {
	if m.listener != nil {
		return m.listener.Addr()
	}
	return nil
}
