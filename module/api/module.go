package api

import (
	"log"
	"net"
	"net/http"
	"sync"

	"github.com/im-pingo/liveforge/core"
)

// Module implements the management API and stream console dashboard.
type Module struct {
	server   *core.Server
	listener net.Listener
	httpSrv  *http.Server
	wg       sync.WaitGroup
}

// NewModule creates a new API module.
func NewModule() *Module {
	return &Module{}
}

// Name returns the module name.
func (m *Module) Name() string { return "api" }

// Init initializes the API HTTP server.
func (m *Module) Init(s *core.Server) error {
	m.server = s
	cfg := s.Config()

	ln, err := net.Listen("tcp", cfg.API.Listen)
	if err != nil {
		return err
	}
	m.listener = ln

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/streams", m.handleStreams)
	mux.HandleFunc("GET /", m.handleConsole)
	m.httpSrv = &http.Server{Handler: mux}

	log.Printf("[api] listening on %s", ln.Addr())

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		if err := m.httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("[api] serve error: %v", err)
		}
	}()

	return nil
}

// Hooks returns the module's event hooks (none for the API module).
func (m *Module) Hooks() []core.HookRegistration { return nil }

// Close shuts down the API server.
func (m *Module) Close() error {
	if m.httpSrv != nil {
		m.httpSrv.Close()
	}
	m.wg.Wait()
	log.Println("[api] stopped")
	return nil
}

// Addr returns the listener address (useful for tests).
func (m *Module) Addr() net.Addr {
	if m.listener != nil {
		return m.listener.Addr()
	}
	return nil
}
