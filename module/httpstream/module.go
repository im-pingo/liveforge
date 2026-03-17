package httpstream

import (
	"log"
	"net"
	"net/http"
	"sync"

	"github.com/im-pingo/liveforge/core"
)

// Module implements the HTTP streaming module for FLV, TS, and FMP4.
type Module struct {
	server   *core.Server
	listener net.Listener
	httpSrv  *http.Server
	wg       sync.WaitGroup

	// Track which streams have muxer callbacks registered.
	registeredMu sync.Mutex
	registered   map[string]bool
}

// NewModule creates a new HTTP streaming module.
func NewModule() *Module {
	return &Module{
		registered: make(map[string]bool),
	}
}

// Name returns the module name.
func (m *Module) Name() string { return "httpstream" }

// Init initializes the HTTP server.
func (m *Module) Init(s *core.Server) error {
	m.server = s
	cfg := s.Config()

	ln, err := net.Listen("tcp", cfg.HTTP.Listen)
	if err != nil {
		return err
	}
	m.listener = ln

	mux := http.NewServeMux()
	mux.HandleFunc("/{path...}", m.handleStream)
	m.httpSrv = &http.Server{Handler: mux}

	log.Printf("[httpstream] listening on %s", ln.Addr())

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		if err := m.httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("[httpstream] serve error: %v", err)
		}
	}()

	return nil
}

// Hooks returns the module's event hooks (none for HTTP streaming).
func (m *Module) Hooks() []core.HookRegistration { return nil }

// Close shuts down the HTTP server.
func (m *Module) Close() error {
	if m.httpSrv != nil {
		m.httpSrv.Close()
	}
	m.wg.Wait()
	log.Println("[httpstream] stopped")
	return nil
}

// Addr returns the listener address (useful for tests).
func (m *Module) Addr() net.Addr {
	if m.listener != nil {
		return m.listener.Addr()
	}
	return nil
}
