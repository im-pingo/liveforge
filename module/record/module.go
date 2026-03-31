package record

import (
	"log/slog"
	"path"
	"sync"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
)

// Module implements stream recording to FLV files.
type Module struct {
	server   *core.Server
	cfg      config.RecordConfig
	mu       sync.Mutex
	sessions map[string]*RecordSession // streamKey -> session
}

// NewModule creates a new record module.
func NewModule() *Module {
	return &Module{
		sessions: make(map[string]*RecordSession),
	}
}

// Name returns the module name.
func (m *Module) Name() string { return "record" }

// Init reads recording config.
func (m *Module) Init(s *core.Server) error {
	m.server = s
	m.cfg = s.Config().Record
	slog.Info("enabled", "module", "record", "pattern", m.cfg.StreamPattern, "format", m.cfg.Format, "path", m.cfg.Path)
	return nil
}

// Hooks returns async hooks for publish start/stop events.
func (m *Module) Hooks() []core.HookRegistration {
	return []core.HookRegistration{
		{
			Event:    core.EventPublish,
			Mode:     core.HookAsync,
			Priority: 50,
			Handler:  m.onPublish,
		},
		{
			Event:    core.EventPublishStop,
			Mode:     core.HookAsync,
			Priority: 50,
			Handler:  m.onPublishStop,
		},
	}
}

// Close stops all active recording sessions.
func (m *Module) Close() error {
	m.mu.Lock()
	sessions := make([]*RecordSession, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.sessions = make(map[string]*RecordSession)
	m.mu.Unlock()

	for _, s := range sessions {
		s.Stop()
	}
	slog.Info("stopped", "module", "record")
	return nil
}

func (m *Module) onPublish(ctx *core.EventContext) error {
	if !matchPattern(m.cfg.StreamPattern, ctx.StreamKey) {
		return nil
	}

	stream, ok := m.server.StreamHub().Find(ctx.StreamKey)
	if !ok {
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.sessions[ctx.StreamKey]; exists {
		return nil // already recording
	}

	session, err := NewRecordSession(ctx.StreamKey, stream, m.cfg)
	if err != nil {
		slog.Error("failed to start session", "module", "record", "stream", ctx.StreamKey, "error", err)
		return nil
	}

	m.sessions[ctx.StreamKey] = session
	go session.Run()
	slog.Info("started recording", "module", "record", "stream", ctx.StreamKey)
	return nil
}

func (m *Module) onPublishStop(ctx *core.EventContext) error {
	m.mu.Lock()
	session, ok := m.sessions[ctx.StreamKey]
	if ok {
		delete(m.sessions, ctx.StreamKey)
	}
	m.mu.Unlock()

	if ok {
		session.Stop()
		slog.Info("stopped recording", "module", "record", "stream", ctx.StreamKey)
	}
	return nil
}

// matchPattern checks if a stream key matches a glob pattern.
// Supports "*" to match everything, "live/*" to match "live/test", etc.
func matchPattern(pattern, key string) bool {
	if pattern == "" || pattern == "*" {
		return true
	}
	matched, _ := path.Match(pattern, key)
	return matched
}
