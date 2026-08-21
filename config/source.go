package config

import (
	"context"
	"errors"
)

var (
	ErrRevisionConflict               = errors.New("config revision conflict")
	ErrSourceReadOnly                 = errors.New("config source is read-only")
	ErrTransactionalUpdateUnsupported = errors.New("config source does not support transactional updates")
)

// Patch is a YAML-shaped merge patch. A nil value removes an override key.
type Patch map[string]any

// Document is one versioned configuration returned by a Source.
type Document struct {
	Config   *Config
	Revision string
	Source   string
}

// Source loads configuration from one backing store.
type Source interface {
	Name() string
	Load(context.Context) (Document, error)
}

// WritableSource persists revision-checked runtime overrides.
type WritableSource interface {
	Source
	Store(context.Context, Patch, string) (string, error)
}

// TransactionalWritableSource can roll back persistence when runtime
// acceptance fails. Manager requires this capability for runtime updates.
type TransactionalWritableSource interface {
	WritableSource
	StoreAndApply(context.Context, Patch, string, func(Document) error) (string, error)
}

// WatchableSource optionally provides change notifications. Manager polling
// remains the fallback and consistency check.
type WatchableSource interface {
	Watch(context.Context, string) (<-chan string, error)
}
