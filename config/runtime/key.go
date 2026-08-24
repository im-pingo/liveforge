package runtime

import (
	"fmt"
	"sync/atomic"

	"github.com/im-pingo/liveforge/config"
)

// Key provides a lock-free read of one typed configuration value.
type Key[T any] struct {
	name  string
	class ChangeClass
	read  func(*config.Config) T
	value atomic.Pointer[T]
}

type typedKeyBinding[T any] struct{ key *Key[T] }

func (b typedKeyBinding[T]) update(snapshot *ConfigSnapshot) {
	value := b.key.read(snapshot.Config)
	b.key.value.Store(&value)
}

// RegisterKey registers a local extractor. Registration never performs source
// I/O; the key is initialized from the current snapshot when one exists.
func RegisterKey[T any](m *Manager, name string, class ChangeClass, read func(*config.Config) T) (*Key[T], error) {
	if m == nil {
		return nil, fmt.Errorf("manager is nil")
	}
	if name == "" {
		return nil, fmt.Errorf("key name is required")
	}
	if read == nil {
		return nil, fmt.Errorf("key reader is required")
	}
	key := &Key[T]{name: name, class: class, read: read}
	m.keysMu.Lock()
	defer m.keysMu.Unlock()
	if _, exists := m.keyNames[name]; exists {
		return nil, fmt.Errorf("config key %q is already registered", name)
	}
	m.keyNames[name] = struct{}{}
	m.keys = append(m.keys, typedKeyBinding[T]{key: key})
	if snapshot := m.active.Load(); snapshot != nil {
		value := read(snapshot.Config)
		key.value.Store(&value)
	}
	return key, nil
}

// Load returns the latest value, or the zero value before the first snapshot.
func (k *Key[T]) Load() T {
	if value := k.value.Load(); value != nil {
		return *value
	}
	var zero T
	return zero
}

// Name returns the registered key name.
func (k *Key[T]) Name() string { return k.name }

// Class returns the key's declared change class.
func (k *Key[T]) Class() ChangeClass { return k.class }
