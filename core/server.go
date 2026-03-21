package core

import (
	"sync/atomic"
	"time"

	"github.com/im-pingo/liveforge/config"
)

// Version is set at build time via -ldflags.
var Version = "dev"

// Server is the main application server that manages modules and lifecycle.
type Server struct {
	config    *config.Config
	eventBus  *EventBus
	hub       *StreamHub
	modules   []Module
	startTime time.Time
	connCount atomic.Int64
	done      chan struct{}
}

// NewServer creates a new Server instance.
func NewServer(cfg *config.Config) *Server {
	bus := NewEventBus()
	return &Server{
		config:    cfg,
		eventBus:  bus,
		hub:       NewStreamHub(cfg.Stream, cfg.Limits, bus),
		startTime: time.Now(),
		done:      make(chan struct{}),
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

// Init initializes all registered modules, registers their hooks, and starts the alive loop.
func (s *Server) Init() error {
	for _, m := range s.modules {
		if err := m.Init(s); err != nil {
			return err
		}
		for _, h := range m.Hooks() {
			s.eventBus.Register(h)
		}
	}

	go s.aliveLoop()

	return nil
}

// Shutdown stops the alive loop and closes all modules in reverse registration order.
func (s *Server) Shutdown() {
	close(s.done)
	for i := len(s.modules) - 1; i >= 0; i-- {
		s.modules[i].Close() //nolint:errcheck
	}
}

// StartTime returns when the server was created.
func (s *Server) StartTime() time.Time {
	return s.startTime
}

// ModuleNames returns the names of all registered modules.
func (s *Server) ModuleNames() []string {
	names := make([]string, len(s.modules))
	for i, m := range s.modules {
		names[i] = m.Name()
	}
	return names
}

// AcquireConn increments the connection counter. Returns false if max_connections is exceeded.
func (s *Server) AcquireConn() bool {
	max := s.config.Limits.MaxConnections
	if max > 0 {
		if s.connCount.Load() >= int64(max) {
			return false
		}
	}
	s.connCount.Add(1)
	return true
}

// ReleaseConn decrements the connection counter.
func (s *Server) ReleaseConn() {
	s.connCount.Add(-1)
}

// ConnectionCount returns the current number of active connections.
func (s *Server) ConnectionCount() int64 {
	return s.connCount.Load()
}

// aliveLoop periodically emits alive events for all active streams.
func (s *Server) aliveLoop() {
	interval := s.config.Notify.AliveInterval
	if interval <= 0 {
		interval = 10 * time.Second
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.emitAliveEvents()
		case <-s.done:
			return
		}
	}
}

func (s *Server) emitAliveEvents() {
	keys := s.hub.Keys()
	for _, key := range keys {
		stream, ok := s.hub.Find(key)
		if !ok || stream.State() == StreamStateDestroying {
			continue
		}

		stats := stream.Stats()
		extra := map[string]any{
			"bytes_in":      stats.BytesIn,
			"video_frames":  stats.VideoFrames,
			"audio_frames":  stats.AudioFrames,
			"bitrate_kbps":  stats.BitrateKbps,
			"fps":           stats.FPS,
			"uptime_sec":    int64(stats.Uptime.Seconds()),
		}

		ctx := &EventContext{StreamKey: key, Extra: extra}

		s.eventBus.Emit(EventStreamAlive, ctx) //nolint:errcheck

		if stream.Publisher() != nil {
			s.eventBus.Emit(EventPublishAlive, ctx) //nolint:errcheck
		}

		subs := stream.Subscribers()
		if len(subs) > 0 {
			s.eventBus.Emit(EventSubscribeAlive, ctx) //nolint:errcheck
		}
	}
}
