package rtmp

import (
	"log/slog"
	"net"
	"sync"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
)

// Module implements core.Module for the RTMP protocol.
type Module struct {
	server   *core.Server
	listener net.Listener
	hub      *core.StreamHub
	eventBus *core.EventBus
	wg       sync.WaitGroup
	closing  chan struct{}

	connsMu sync.Mutex
	conns   map[net.Conn]struct{}
}

// NewModule creates a new RTMP module.
func NewModule() *Module {
	return &Module{
		closing: make(chan struct{}),
		conns:   make(map[net.Conn]struct{}),
	}
}

// Name returns the module name.
func (m *Module) Name() string { return "rtmp" }

// Init initializes the RTMP module and starts the TCP listener.
func (m *Module) Init(s *core.Server) error {
	m.server = s
	m.eventBus = s.GetEventBus()

	cfg := s.Config()
	m.hub = s.StreamHub()

	ln, err := s.MakeListener(cfg.RTMP.Listen, cfg.RTMP.TLS)
	if err != nil {
		return err
	}
	m.listener = ln

	proto := "RTMP"
	if cfg.TLS.Configured() && (cfg.RTMP.TLS == nil || *cfg.RTMP.TLS) {
		proto = "RTMPS"
	}
	slog.Info("listening", "module", "rtmp", "proto", proto, "addr", cfg.RTMP.Listen)

	m.wg.Add(1)
	go m.acceptLoop(cfg.RTMP.ChunkSize, cfg.RTMP.SkipTracker)

	return nil
}

// Hooks returns hook registrations (none for RTMP).
func (m *Module) Hooks() []core.HookRegistration { return nil }

// Close stops the RTMP module.
func (m *Module) Close() error {
	close(m.closing)
	if m.listener != nil {
		m.listener.Close()
	}
	// Close all active connections to unblock handlers blocked on Read.
	m.connsMu.Lock()
	for c := range m.conns {
		c.Close()
	}
	m.connsMu.Unlock()

	m.wg.Wait()
	slog.Info("stopped", "module", "rtmp")
	return nil
}

// Hub returns the stream hub managed by this module.
func (m *Module) Hub() *core.StreamHub { return m.hub }

func (m *Module) acceptLoop(chunkSize int, skipCfg *config.SkipTrackerConfig) {
	defer m.wg.Done()

	for {
		conn, err := m.listener.Accept()
		if err != nil {
			select {
			case <-m.closing:
				return
			default:
				slog.Error("accept error", "module", "rtmp", "error", err)
				continue
			}
		}

		if !m.server.AcquireConn() {
			slog.Warn("max connections reached", "module", "rtmp", "remote", conn.RemoteAddr())
			conn.Close()
			continue
		}
		m.wg.Add(1)
		go m.handleConn(conn, chunkSize, skipCfg)
	}
}

func (m *Module) handleConn(conn net.Conn, chunkSize int, skipCfg *config.SkipTrackerConfig) {
	defer m.wg.Done()
	defer m.server.ReleaseConn()

	m.connsMu.Lock()
	m.conns[conn] = struct{}{}
	m.connsMu.Unlock()
	defer func() {
		m.connsMu.Lock()
		delete(m.conns, conn)
		m.connsMu.Unlock()
	}()

	if err := ServerHandshake(conn); err != nil {
		slog.Error("handshake failed", "module", "rtmp", "error", err)
		conn.Close()
		return
	}

	handler := NewHandler(conn, m.hub, m.eventBus, chunkSize, skipCfg)
	if err := handler.Handle(); err != nil {
		slog.Error("handler error", "module", "rtmp", "error", err)
	}
}
