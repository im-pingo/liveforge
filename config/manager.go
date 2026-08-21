package config

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

type Snapshot struct {
	Desired        *Config
	Effective      *Config
	Revision       string
	Source         string
	LoadedAt       time.Time
	PendingRestart []string
}

type ApplyResult struct {
	Changed        bool
	Revision       string
	PendingRestart []string
	Snapshot       Snapshot
}

type ApplyFunc func(context.Context, *Config, *Config, ChangeSet) error

// Manager periodically refreshes one Source and serves immutable cached
// snapshots to all runtime consumers.
type Manager struct {
	source    Source
	interval  time.Duration
	apply     ApplyFunc
	current   atomic.Pointer[Snapshot]
	refreshMu sync.Mutex
}

func NewManager(source Source, interval time.Duration, apply ApplyFunc) *Manager {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	return &Manager{source: source, interval: interval, apply: apply}
}

func (m *Manager) SetApply(apply ApplyFunc) {
	m.refreshMu.Lock()
	m.apply = apply
	m.refreshMu.Unlock()
}

// Current returns a caller-owned deep copy without reading the Source.
func (m *Manager) Current() Snapshot {
	snapshot := m.current.Load()
	if snapshot == nil {
		return Snapshot{}
	}
	return cloneSnapshot(*snapshot)
}

func (m *Manager) Refresh(ctx context.Context) (ApplyResult, error) {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()

	doc, err := m.source.Load(ctx)
	if err != nil {
		return ApplyResult{}, err
	}
	return m.refreshDocumentLocked(ctx, doc)
}

func (m *Manager) refreshDocumentLocked(ctx context.Context, doc Document) (ApplyResult, error) {
	desired := CloneConfig(doc.Config)
	if desired != nil {
		normalize(desired)
	}
	if err := Validate(desired); err != nil {
		return ApplyResult{}, fmt.Errorf("validate config revision %s: %w", doc.Revision, err)
	}
	previous := m.current.Load()
	if previous != nil && previous.Revision == doc.Revision {
		return applyResult(false, *previous), nil
	}

	effective := CloneConfig(desired)
	changes := ChangeSet{}
	var err error
	if previous != nil {
		changes, err = diffConfigs(previous.Effective, desired)
		if err != nil {
			return ApplyResult{}, err
		}
		effective, err = buildEffective(previous.Effective, desired, changes)
		if err != nil {
			return ApplyResult{}, err
		}
		if m.apply != nil {
			if err := m.apply(ctx, CloneConfig(previous.Effective), CloneConfig(effective), cloneChangeSet(changes)); err != nil {
				return ApplyResult{}, fmt.Errorf("apply config revision %s: %w", doc.Revision, err)
			}
		}
	}
	pending := changes.Paths(ReloadRestart)
	snapshot := &Snapshot{
		Desired: desired, Effective: effective, Revision: doc.Revision,
		Source: doc.Source, LoadedAt: time.Now(), PendingRestart: pending,
	}
	m.current.Store(snapshot)
	return applyResult(true, *snapshot), nil
}

func cloneSnapshot(snapshot Snapshot) Snapshot {
	return Snapshot{
		Desired: CloneConfig(snapshot.Desired), Effective: CloneConfig(snapshot.Effective),
		Revision: snapshot.Revision, Source: snapshot.Source, LoadedAt: snapshot.LoadedAt,
		PendingRestart: append([]string(nil), snapshot.PendingRestart...),
	}
}

func applyResult(changed bool, snapshot Snapshot) ApplyResult {
	return ApplyResult{
		Changed: changed, Revision: snapshot.Revision,
		PendingRestart: append([]string(nil), snapshot.PendingRestart...),
		Snapshot:       cloneSnapshot(snapshot),
	}
}

func cloneChangeSet(changes ChangeSet) ChangeSet {
	cloned := make(ChangeSet, len(changes))
	for path, class := range changes {
		cloned[path] = class
	}
	return cloned
}

func (m *Manager) Update(ctx context.Context, patch Patch, expectedRevision string) (ApplyResult, error) {
	_, ok := m.source.(WritableSource)
	if !ok {
		return ApplyResult{}, ErrSourceReadOnly
	}
	transactional, ok := m.source.(TransactionalWritableSource)
	if !ok {
		return ApplyResult{}, ErrTransactionalUpdateUnsupported
	}
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()

	var result ApplyResult
	_, err := transactional.StoreAndApply(ctx, patch, expectedRevision, func(doc Document) error {
		var applyErr error
		result, applyErr = m.refreshDocumentLocked(ctx, doc)
		return applyErr
	})
	return result, err
}

func (m *Manager) Run(ctx context.Context, onError func(error)) {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := m.Refresh(ctx); err != nil && !errors.Is(err, context.Canceled) && onError != nil {
				onError(err)
			}
		}
	}
}
