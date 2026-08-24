package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/im-pingo/liveforge/config"
)

type keyBinding interface{ update(*ConfigSnapshot) }

// Manager owns source I/O and publishes immutable configuration snapshots.
type Manager struct {
	source       ConfigSource
	sourceName   string
	pollInterval time.Duration
	loadTimeout  time.Duration
	onChange     func(ChangeSet) error

	active atomic.Pointer[ConfigSnapshot]

	mu            sync.Mutex
	started       bool
	closed        bool
	cancel        context.CancelFunc
	workerDone    chan struct{}
	initialResult chan error
	refreshCh     chan struct{}
	closeOnce     sync.Once

	statusMu sync.RWMutex
	status   Status

	keysMu   sync.Mutex
	keys     []keyBinding
	keyNames map[string]struct{}

	callbackCh   chan ChangeSet
	callbackDone chan struct{}
}

// NewManager validates options and seeds the optional bootstrap snapshot.
func NewManager(opts Options) (*Manager, error) {
	if opts.Source == nil {
		return nil, ErrInvalidSource
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = 30 * time.Second
	}
	if opts.PollInterval < time.Second {
		opts.PollInterval = time.Second
	}
	if opts.LoadTimeout <= 0 {
		opts.LoadTimeout = 10 * time.Second
	}
	if opts.CallbackBuffer <= 0 {
		opts.CallbackBuffer = 16
	}
	name := opts.SourceName
	if name == "" {
		if named, ok := opts.Source.(NamedSource); ok {
			name = named.Name()
		}
	}
	if name == "" {
		name = "custom"
	}
	m := &Manager{
		source:        opts.Source,
		sourceName:    name,
		pollInterval:  opts.PollInterval,
		loadTimeout:   opts.LoadTimeout,
		onChange:      opts.OnChange,
		workerDone:    make(chan struct{}),
		initialResult: make(chan error, 1),
		refreshCh:     make(chan struct{}, 1),
		callbackCh:    make(chan ChangeSet, opts.CallbackBuffer),
		callbackDone:  make(chan struct{}),
		keyNames:      make(map[string]struct{}),
		status:        Status{Source: name},
	}
	if opts.Initial != nil {
		if err := validateConfig(opts.Initial); err != nil {
			return nil, fmt.Errorf("invalid initial config: %w", err)
		}
		cfg, err := cloneConfig(opts.Initial)
		if err != nil {
			return nil, err
		}
		hash, err := configHash(cfg)
		if err != nil {
			return nil, err
		}
		m.active.Store(&ConfigSnapshot{Config: cfg, DesiredConfig: cfg, Version: Version{Hash: hash}, Source: name, LoadedAt: time.Now()})
		m.status.ActiveVersion = Version{Hash: hash}
	}
	go m.callbackLoop()
	return m, nil
}

// Start launches the worker. With no initial config it waits for the first
// valid source snapshot; a bootstrap config allows startup to continue while a
// remote source is refreshed in the background.
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrClosed
	}
	if m.started {
		m.mu.Unlock()
		return nil
	}
	workerCtx, cancel := context.WithCancel(ctx)
	m.cancel = cancel
	m.started = true
	m.mu.Unlock()
	go m.run(workerCtx)
	if m.active.Load() != nil {
		return nil
	}
	select {
	case err := <-m.initialResult:
		if err != nil {
			_ = m.Close()
			return err
		}
		return nil
	case <-ctx.Done():
		_ = m.Close()
		return ctx.Err()
	}
}

func (m *Manager) run(ctx context.Context) {
	defer close(m.workerDone)
	err := m.load(ctx)
	if m.active.Load() == nil {
		m.initialResult <- errOrInitial(err)
		if err != nil {
			return
		}
	} else {
		m.initialResult <- nil
	}
	if err != nil && !isContextError(err) {
		slog.Warn("runtime config source initial refresh failed; keeping bootstrap config", "source", m.sourceName, "error", err)
	}
	ticker := time.NewTicker(m.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = m.load(ctx)
		case <-m.refreshCh:
			_ = m.load(ctx)
		}
	}
}

func errOrInitial(err error) error {
	if err != nil {
		return err
	}
	return ErrNoInitial
}

func (m *Manager) load(parent context.Context) error {
	if parent.Err() != nil {
		return parent.Err()
	}
	previous := Version{}
	if current := m.active.Load(); current != nil {
		previous = current.Version
	}
	ctx, cancel := context.WithTimeout(parent, m.loadTimeout)
	defer cancel()
	m.setAttempt()
	result, err := m.source.Load(ctx, previous)
	if err != nil {
		m.setFailure(err)
		return err
	}
	if len(result.Data) == 0 && result.Version != "" && result.Version == previous.Value {
		m.setSuccess()
		return nil
	}
	cfg, err := ParseDocument(result.Data)
	if err != nil {
		m.setFailure(err)
		return err
	}
	hash, err := configHash(cfg)
	if err != nil {
		m.setFailure(err)
		return err
	}
	if previous.Hash != "" && previous.Hash == hash {
		m.setSuccess()
		return nil
	}
	old := m.active.Load()
	changes, err := diffConfigs(snapshotDesiredConfig(old), cfg)
	if err != nil {
		m.setFailure(err)
		return err
	}
	for _, change := range changes {
		if change.Class == ChangeImmutable {
			err := fmt.Errorf("%w: %s", ErrImmutableChange, change.Path)
			m.setFailure(err)
			return err
		}
	}
	owned, err := cloneConfig(cfg)
	if err != nil {
		m.setFailure(err)
		return err
	}
	version := Version{Value: result.Version, Hash: hash}
	if version.Value == "" {
		version.Value = hash
	}
	applied, err := applyHotChanges(snapshotConfig(old), owned, changes)
	if err != nil {
		m.setFailure(err)
		return err
	}
	pendingChanges, err := diffConfigs(applied, owned)
	if err != nil {
		m.setFailure(err)
		return err
	}
	pending := make([]string, 0)
	for _, change := range pendingChanges {
		if change.Class == ChangeRestart {
			pending = append(pending, change.Path)
		}
	}
	next := &ConfigSnapshot{Config: applied, DesiredConfig: owned, Version: version, Source: m.sourceName, LoadedAt: time.Now(), LastModified: result.LastModified, Changes: changes, PendingRestart: pending}
	m.active.Store(next)
	m.updateKeys(next)
	m.setSuccessVersion(version, pending)
	if len(changes) > 0 && m.onChange != nil {
		set := ChangeSet{Previous: previous, Current: version, Changes: append([]Change(nil), changes...), Restart: append([]string(nil), pending...)}
		select {
		case m.callbackCh <- set:
		default:
			m.incrementDropped()
		}
	}
	return nil
}

func snapshotConfig(s *ConfigSnapshot) *config.Config {
	if s == nil {
		return nil
	}
	return s.Config
}

func snapshotDesiredConfig(s *ConfigSnapshot) *config.Config {
	if s == nil {
		return nil
	}
	if s.DesiredConfig != nil {
		return s.DesiredConfig
	}
	return s.Config
}

func (m *Manager) Refresh(ctx context.Context) error {
	m.mu.Lock()
	started, closed := m.started, m.closed
	m.mu.Unlock()
	if closed {
		return ErrClosed
	}
	if !started {
		return ErrNotStarted
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	select {
	case m.refreshCh <- struct{}{}:
	default:
	}
	return nil
}

func (m *Manager) Snapshot() *ConfigSnapshot { return m.active.Load() }

func (m *Manager) Status() Status {
	m.statusMu.RLock()
	defer m.statusMu.RUnlock()
	out := m.status
	out.PendingRestart = append([]string(nil), out.PendingRestart...)
	return out
}

func (m *Manager) Close() error {
	var err error
	m.closeOnce.Do(func() {
		m.mu.Lock()
		m.closed = true
		started := m.started
		cancel := m.cancel
		m.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		if started {
			<-m.workerDone
		}
		close(m.callbackCh)
		<-m.callbackDone
		err = m.source.Close()
	})
	return err
}

func (m *Manager) callbackLoop() {
	defer close(m.callbackDone)
	for set := range m.callbackCh {
		if m.onChange == nil {
			continue
		}
		if err := m.onChange(set); err != nil {
			m.statusMu.Lock()
			m.status.CallbackFailures++
			m.statusMu.Unlock()
			slog.Error("runtime config callback failed", "error", err)
		}
	}
}

func (m *Manager) updateKeys(snapshot *ConfigSnapshot) {
	m.keysMu.Lock()
	defer m.keysMu.Unlock()
	for _, key := range m.keys {
		key.update(snapshot)
	}
}

func (m *Manager) setAttempt() {
	m.statusMu.Lock()
	m.status.LastAttempt = time.Now()
	m.statusMu.Unlock()
}

func (m *Manager) setSuccess() {
	m.statusMu.Lock()
	m.status.LastSuccess = time.Now()
	m.status.ConsecutiveFailures = 0
	m.status.LastError = ""
	m.statusMu.Unlock()
}

func (m *Manager) setSuccessVersion(v Version, pending []string) {
	m.statusMu.Lock()
	m.status.ActiveVersion = v
	m.status.LastSuccess = time.Now()
	m.status.ConsecutiveFailures = 0
	m.status.LastError = ""
	m.status.PendingRestart = append([]string(nil), pending...)
	m.statusMu.Unlock()
}

func (m *Manager) setFailure(err error) {
	m.statusMu.Lock()
	m.status.ConsecutiveFailures++
	m.status.LastError = err.Error()
	m.statusMu.Unlock()
}

func (m *Manager) incrementDropped() {
	m.statusMu.Lock()
	m.status.DroppedCallbacks++
	m.statusMu.Unlock()
}

func isContextError(err error) bool {
	return err == context.Canceled || err == context.DeadlineExceeded
}
