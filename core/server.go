package core

import "github.com/im-pingo/liveforge/config"

// Server is the main application server that manages modules and lifecycle.
type Server struct {
	config   *config.Config
	eventBus *EventBus
	hub      *StreamHub
	modules  []Module
}

// NewServer creates a new Server instance.
func NewServer(cfg *config.Config) *Server {
	bus := NewEventBus()
	return &Server{
		config:   cfg,
		eventBus: bus,
		hub:      NewStreamHub(cfg.Stream, bus),
	}
}

// Config returns the server configuration.
func (s *Server) Config() *config.Config {
	return s.config
}

// GetEventBus returns the server's event bus.
func (s *Server) GetEventBus() *EventBus {
	return s.eventBus
}

// StreamHub returns the shared stream hub.
func (s *Server) StreamHub() *StreamHub {
	return s.hub
}

// RegisterModule adds a module to the server.
func (s *Server) RegisterModule(m Module) {
	s.modules = append(s.modules, m)
}

// Init initializes all registered modules and registers their hooks.
func (s *Server) Init() error {
	for _, m := range s.modules {
		if err := m.Init(s); err != nil {
			return err
		}
		for _, h := range m.Hooks() {
			s.eventBus.Register(h)
		}
	}
	return nil
}

// Shutdown closes all modules in reverse registration order.
func (s *Server) Shutdown() {
	for i := len(s.modules) - 1; i >= 0; i-- {
		s.modules[i].Close() //nolint:errcheck
	}
}
