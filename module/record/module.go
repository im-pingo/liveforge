package record

import (
	"context"
	"log/slog"
	"path"
	"sync"
	"sync/atomic"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
)

// Module implements stream recording to FLV files.
type Module struct {
	server   *core.Server
	runtime  atomic.Pointer[recordRuntime]
	mu       sync.Mutex
	sessions map[string]*RecordSession // streamKey -> session
	history  []RecordingSessionStatus
	metrics  RecordingMetrics
}

type recordRuntime struct {
	cfg      config.RecordConfig
	storage  Storage
	template string
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
	cfg := s.Config().Record
	storage, template, err := newStorageForConfig(cfg)
	if err != nil {
		return err
	}
	m.runtime.Store(&recordRuntime{cfg: cfg, storage: storage, template: template})
	slog.Info("enabled", "module", "record", "pattern", cfg.StreamPattern, "format", cfg.Format, "path", cfg.Path)
	return nil
}

// OnReload atomically applies recording policy for new sessions. Active
// sessions retain their creation policy so their current file finishes safely.
func (m *Module) OnReload(s *core.Server) error {
	cfg := s.Config().Record
	if current := m.runtime.Load(); current != nil {
		cfg.Enabled = current.cfg.Enabled
	}
	storage, template, err := newStorageForConfig(cfg)
	if err != nil {
		return err
	}
	m.runtime.Store(&recordRuntime{cfg: cfg, storage: storage, template: template})
	return nil
}

// Policy returns a copy of the active recording policy.
func (m *Module) Policy() config.RecordConfig {
	if runtime := m.runtime.Load(); runtime != nil {
		return runtime.cfg
	}
	return config.RecordConfig{}
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
		s.Wait()
	}
	slog.Info("stopped", "module", "record")
	return nil
}

func (m *Module) onPublish(ctx *core.EventContext) error {
	runtime := m.runtime.Load()
	if runtime == nil {
		return nil
	}
	cfg := runtime.cfg
	if !matchPattern(cfg.StreamPattern, ctx.StreamKey) {
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

	session, err := newRecordSession(ctx.StreamKey, stream, cfg, runtime.storage, runtime.template, &m.metrics)
	if err != nil {
		slog.Error("failed to start session", "module", "record", "stream", ctx.StreamKey, "error", err)
		return nil
	}

	m.sessions[ctx.StreamKey] = session
	session.onComplete = func(status RecordingSessionStatus) {
		m.mu.Lock()
		if m.sessions[ctx.StreamKey] == session {
			delete(m.sessions, ctx.StreamKey)
		}
		m.history = append(m.history, status)
		if len(m.history) > 100 {
			m.history = append([]RecordingSessionStatus(nil), m.history[len(m.history)-100:]...)
		}
		m.mu.Unlock()
	}
	go session.Run()
	slog.Info("started recording", "module", "record", "stream", ctx.StreamKey)
	return nil
}

// ListRecordings returns completed and preserved failed local recordings.
func (m *Module) ListRecordings(ctx context.Context) ([]RecordingInfo, error) {
	runtime := m.runtime.Load()
	if runtime == nil || runtime.storage == nil {
		return nil, nil
	}
	return runtime.storage.List(ctx)
}

// Recording returns metadata for one safe storage-relative recording ID.
func (m *Module) Recording(ctx context.Context, id string) (RecordingInfo, error) {
	runtime := m.runtime.Load()
	if runtime == nil || runtime.storage == nil {
		return RecordingInfo{}, ErrRecordingNotFound
	}
	return runtime.storage.Stat(ctx, id)
}

// OpenRecording opens one finalized recording for download.
func (m *Module) OpenRecording(ctx context.Context, id string) (ReadSeekCloser, RecordingInfo, error) {
	runtime := m.runtime.Load()
	if runtime == nil || runtime.storage == nil {
		return nil, RecordingInfo{}, ErrRecordingNotFound
	}
	return runtime.storage.Open(ctx, id)
}

// DeleteRecording deletes one finalized recording and its metadata.
func (m *Module) DeleteRecording(ctx context.Context, id string) error {
	runtime := m.runtime.Load()
	if runtime == nil || runtime.storage == nil {
		return ErrRecordingNotFound
	}
	err := runtime.storage.Delete(ctx, id)
	if err == nil {
		m.metrics.deleted.Add(1)
	}
	return err
}

// RecordingStatus exposes bounded session history, storage health and metrics.
func (m *Module) RecordingStatus(ctx context.Context) RecordingStatusSnapshot {
	m.mu.Lock()
	sessions := make([]RecordingSessionStatus, 0, len(m.sessions)+len(m.history))
	for _, session := range m.sessions {
		sessions = append(sessions, session.Status())
	}
	sessions = append(sessions, m.history...)
	m.mu.Unlock()
	status := RecordingStatusSnapshot{Sessions: sessions, Metrics: m.metrics.Snapshot()}
	if runtime := m.runtime.Load(); runtime != nil && runtime.storage != nil {
		status.Storage = runtime.storage.Health(ctx)
	}
	return status
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
		session.Wait()
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

var _ core.Reloadable = (*Module)(nil)
