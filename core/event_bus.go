package core

import (
	"sort"
	"sync"
)

// EventBus dispatches events to registered hook handlers.
type EventBus struct {
	mu    sync.RWMutex
	hooks map[EventType][]HookRegistration
}

// NewEventBus creates a new EventBus.
func NewEventBus() *EventBus {
	return &EventBus{
		hooks: make(map[EventType][]HookRegistration),
	}
}

// Register adds a hook registration, maintaining priority order.
func (b *EventBus) Register(h HookRegistration) {
	b.mu.Lock()
	defer b.mu.Unlock()

	hooks := append(b.hooks[h.Event], h)
	sort.Slice(hooks, func(i, j int) bool {
		return hooks[i].Priority < hooks[j].Priority
	})
	b.hooks[h.Event] = hooks
}

// Emit dispatches an event to all registered handlers.
// Sync hooks run in priority order; if any returns an error, execution stops
// and that error is returned. Async hooks fire in goroutines after all sync
// hooks succeed.
func (b *EventBus) Emit(event EventType, ctx *EventContext) error {
	b.mu.RLock()
	hooks := b.hooks[event]
	// Copy slice under lock to avoid races if Register is called concurrently
	copied := make([]HookRegistration, len(hooks))
	copy(copied, hooks)
	b.mu.RUnlock()

	var asyncHooks []HookRegistration

	for _, h := range copied {
		if h.Mode == HookAsync {
			asyncHooks = append(asyncHooks, h)
			continue
		}
		if err := h.Handler(ctx); err != nil {
			return err
		}
	}

	// Fire async hooks in goroutines
	for _, h := range asyncHooks {
		go h.Handler(ctx) //nolint:errcheck
	}

	return nil
}
