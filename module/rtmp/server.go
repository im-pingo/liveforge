package rtmp

import (
	"log"
	"net"
	"sync"

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
}

// NewModule creates a new RTMP module.
func NewModule() *Module {
	return &Module{
		closing: make(chan struct{}),
	}
}

// Name returns the module name.
func (m *Module) Name() string { return "rtmp" }

// Init initializes the RTMP module and starts the TCP listener.
func (m *Module) Init(s *core.Server) error {
	m.server = s
	m.eventBus = s.GetEventBus()

	cfg := s.Config()
	m.hub = core.NewStreamHub(cfg.Stream, m.eventBus)

	ln, err := net.Listen("tcp", cfg.RTMP.Listen)
	if err != nil {
		return err
	}
	m.listener = ln

	log.Printf("RTMP listening on %s", cfg.RTMP.Listen)

	m.wg.Add(1)
	go m.acceptLoop(cfg.RTMP.ChunkSize)

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
	m.wg.Wait()
	log.Println("RTMP module stopped")
	return nil
}

// Hub returns the stream hub managed by this module.
func (m *Module) Hub() *core.StreamHub { return m.hub }

func (m *Module) acceptLoop(chunkSize int) {
	defer m.wg.Done()

	for {
		conn, err := m.listener.Accept()
		if err != nil {
			select {
			case <-m.closing:
				return
			default:
				log.Printf("RTMP accept error: %v", err)
				continue
			}
		}

		m.wg.Add(1)
		go m.handleConn(conn, chunkSize)
	}
}

func (m *Module) handleConn(conn net.Conn, chunkSize int) {
	defer m.wg.Done()

	if err := ServerHandshake(conn); err != nil {
		log.Printf("RTMP handshake failed: %v", err)
		conn.Close()
		return
	}

	handler := NewHandler(conn, m.hub, m.eventBus, chunkSize)
	if err := handler.Handle(); err != nil {
		log.Printf("RTMP handler error: %v", err)
	}
}
