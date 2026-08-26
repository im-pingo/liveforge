package record

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"sync"
	"sync/atomic"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
)

// Module implements stream recording to FLV files.
type Module struct {
	server    *core.Server
	runtime   atomic.Pointer[recordRuntime]
	mu        sync.Mutex
	sessions  map[string]*RecordSession // streamKey -> session
	history   []RecordingSessionStatus
	metrics   RecordingMetrics
	wg        sync.WaitGroup
	closing   bool
	closeOnce sync.Once
	closeDone chan struct{}
	closeErr  error
}

type recordRuntime struct {
	cfg      config.RecordConfig
	storage  Storage
	template string
}

// NewModule creates a new record module.
func NewModule() *Module {
	return &Module{
		sessions:  make(map[string]*RecordSession),
		closeDone: make(chan struct{}),
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
	commit, err := m.PrepareReload(s)
	if err != nil {
		return err
	}
	commit()
	return nil
}

// PrepareReload validates and constructs recording storage before any module
// publishes candidate policy. The commit is an atomic pointer swap.
func (m *Module) PrepareReload(s *core.Server) (func(), error) {
	cfg := s.Config().Record
	if current := m.runtime.Load(); current != nil {
		cfg.Enabled = current.cfg.Enabled
		cfg.Path = current.cfg.Path
		next := &recordRuntime{cfg: cfg, storage: current.storage, template: current.template}
		return func() { m.runtime.Store(next) }, nil
	}
	storage, template, err := newStorageForConfig(cfg)
	if err != nil {
		return nil, err
	}
	next := &recordRuntime{cfg: cfg, storage: storage, template: template}
	return func() { m.runtime.Store(next) }, nil
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
			Consumer: "record",
			Handler:  m.onPublish,
		},
		{
			Event:    core.EventPublishStop,
			Mode:     core.HookAsync,
			Priority: 50,
			Consumer: "record",
			Handler:  m.onPublishStop,
		},
	}
}

// Close stops all active recording sessions.
func (m *Module) Close() error {
	m.closeOnce.Do(func() {
		m.closeErr = m.close()
		close(m.closeDone)
	})
	<-m.closeDone
	return m.closeErr
}

func (m *Module) close() error {
	m.mu.Lock()
	m.closing = true
	sessions := make([]*RecordSession, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
		s.Stop()
	}
	m.sessions = make(map[string]*RecordSession)
	m.mu.Unlock()

	deadline := time.Now().Add(m.drainTimeout())
	pending := 0
	for _, session := range sessions {
		if !session.WaitUntil(deadline) {
			pending++
		}
	}
	workersDone := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(workersDone)
	}()
	var workerErr error
	remaining := time.Until(deadline)
	if remaining <= 0 {
		workerErr = fmt.Errorf("record: drain timeout waiting for module workers")
	} else {
		timer := time.NewTimer(remaining)
		select {
		case <-workersDone:
			if !timer.Stop() {
				<-timer.C
			}
		case <-timer.C:
			workerErr = fmt.Errorf("record: drain timeout waiting for module workers")
		}
	}
	if runtime := m.runtime.Load(); runtime != nil {
		if closer, ok := runtime.storage.(interface{ Close() error }); ok {
			_ = closer.Close()
		}
	}
	slog.Info("stopped", "module", "record")
	if pending > 0 {
		return errors.Join(fmt.Errorf("record: drain timeout with %d session(s) still finalizing", pending), workerErr)
	}
	return workerErr
}

func (m *Module) drainTimeout() time.Duration {
	if m.server != nil {
		if timeout := m.server.Config().Server.DrainTimeout; timeout > 0 {
			return timeout
		}
	}
	return 10 * time.Second
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
	if m.closing {
		m.mu.Unlock()
		return nil
	}
	m.wg.Add(1)
	m.mu.Unlock()
	defer m.wg.Done()
	publisherID := ctx.PublisherID

	for {
		if publisher := stream.Publisher(); publisher != nil {
			if publisherID == "" {
				publisherID = publisher.ID()
			} else if publisher.ID() != publisherID {
				return nil
			}
		}
		m.mu.Lock()
		if m.closing {
			m.mu.Unlock()
			return nil
		}
		if existing := m.sessions[ctx.StreamKey]; existing != nil {
			if existing.publisherID == publisherID {
				m.mu.Unlock()
				return nil
			}
			existing.Stop()
			m.mu.Unlock()
			existing.Wait()
			continue
		}

		session, err := newRecordSession(ctx.StreamKey, stream, cfg, runtime.storage, runtime.template, &m.metrics)
		if err != nil {
			m.mu.Unlock()
			slog.Error("failed to start session", "module", "record", "stream", ctx.StreamKey, "error", err)
			return nil
		}
		session.publisherID = publisherID

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
		m.mu.Unlock()
		go session.Run()
		slog.Info("started recording", "module", "record", "stream", ctx.StreamKey)
		return nil
	}
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
	status := RecordingStatusSnapshot{
		Enabled:   true,
		Available: true,
		State:     RecordingReady,
		Sessions:  sessions,
		Metrics:   m.metrics.Snapshot(),
	}
	if runtime := m.runtime.Load(); runtime != nil && runtime.storage != nil {
		status.Storage = runtime.storage.Health(ctx)
		switch {
		case status.Storage.Error != "":
			status.Available = false
			status.State = RecordingUnavailable
			status.Reason = status.Storage.Error
		case status.Storage.LowSpace:
			status.State = RecordingDegraded
			status.Reason = "recording storage is low on space"
		}
	} else {
		status.Available = false
		status.State = RecordingUnavailable
		status.Reason = "recording storage unavailable"
	}
	return status
}

func (m *Module) onPublishStop(ctx *core.EventContext) error {
	m.mu.Lock()
	session := m.sessions[ctx.StreamKey]
	if session != nil && ctx.PublisherID != "" && session.publisherID != "" && session.publisherID != ctx.PublisherID {
		session = nil
	}
	if session != nil {
		session.Stop()
	}
	m.mu.Unlock()

	if session != nil {
		if session.WaitUntil(time.Now().Add(m.drainTimeout())) {
			slog.Info("stopped recording", "module", "record", "stream", ctx.StreamKey)
		}
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
