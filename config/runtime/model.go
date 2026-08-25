// Package runtime provides background configuration refresh with lock-free reads.
package runtime

import (
	"context"
	"errors"
	"time"

	"github.com/im-pingo/liveforge/config"
)

var (
	ErrClosed          = errors.New("config runtime manager is closed")
	ErrNotStarted      = errors.New("config runtime manager is not started")
	ErrNoInitial       = errors.New("config runtime source did not produce an initial snapshot")
	ErrInvalidSource   = errors.New("config runtime source is nil")
	ErrImmutableChange = errors.New("immutable configuration change")
)

// ConfigSource loads a complete configuration document. Implementations must not
// retain or mutate the returned byte slice after Load returns.
type ConfigSource interface {
	Load(ctx context.Context, previous Version) (Snapshot, error)
	Close() error
}

// NamedSource is an optional extension used for status reporting.
type NamedSource interface {
	ConfigSource
	Name() string
}

// Version identifies the source revision and normalized content hash.
type Version struct {
	Value        string
	Hash         string
	ETag         string
	LastModified time.Time
}

// Snapshot is the source result before parsing and validation.
type Snapshot struct {
	Data         []byte
	Version      string
	ETag         string
	LastModified time.Time
}

// ChangeClass describes whether a configuration path can be applied in place.
type ChangeClass string

const (
	ChangeHot       ChangeClass = "hot_reload"
	ChangeRestart   ChangeClass = "restart_required"
	ChangeImmutable ChangeClass = "immutable"
)

// Change identifies one changed configuration path.
type Change struct {
	Path  string
	Class ChangeClass
}

// ChangeSet describes one accepted configuration transition.
type ChangeSet struct {
	Previous Version
	Current  Version
	Changes  []Change
	Restart  []string
}

// ConfigSnapshot is immutable after publication. Config is owned by the
// manager; consumers must treat it as read-only.
type ConfigSnapshot struct {
	// Config is the effective runtime view. Restart-required values remain at
	// their last applied values until the process restarts.
	Config *config.Config
	// DesiredConfig retains the latest valid source values, including changes
	// that are waiting for a restart. Consumers should normally read Config.
	DesiredConfig  *config.Config
	Version        Version
	Source         string
	LoadedAt       time.Time
	LastModified   time.Time
	Changes        []Change
	PendingRestart []string
}

// Status is a point-in-time copy of manager health. Error text is source
// generated and must never contain credentials.
type Status struct {
	Source                         string
	ActiveVersion                  Version
	LastAttempt                    time.Time
	LastSuccess                    time.Time
	ConsecutiveFailures            uint64
	LastError                      string
	PendingRestart                 []string
	CallbackFailures               uint64
	DroppedCallbacks               uint64
	ConfigChangesAccepted          uint64
	ConfigChangesRejected          uint64
	ConfigChangesApplicationFailed uint64
}

// Options controls a Manager.
type Options struct {
	Source         ConfigSource
	SourceName     string
	PollInterval   time.Duration
	LoadTimeout    time.Duration
	Initial        *config.Config
	CallbackBuffer int
	// Apply validates and applies a candidate on the manager's background
	// worker before it becomes visible to snapshot and typed-key readers.
	Apply func(*ConfigSnapshot, ChangeSet) error
	// OnChange observes accepted transitions asynchronously. Notifications are
	// coalesced under load, but the latest accepted transition is never dropped.
	OnChange func(ChangeSet) error
}
