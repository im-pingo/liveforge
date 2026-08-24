package core

import (
	"sort"
	"sync"
)

// EventBus dispatches events to registered hook handlers.
type EventBus struct {
	mu    sync.RWMutex
	hooks map[EventType][]HookRegistration

	asyncMu        sync.Mutex
	lifecycleLanes map[lifecycleLaneKey]*lifecycleLane
}

type lifecycleLaneKey struct {
	streamKey string
	clientID  string
	publish   bool
}

type lifecycleDispatch struct {
	ctx   *EventContext
	hooks []HookRegistration
}

type lifecycleLane struct {
	queue   []lifecycleDispatch
	running bool
}

// NewEventBus creates a new EventBus.
func NewEventBus() *EventBus {
	return &EventBus{
		hooks:          make(map[EventType][]HookRegistration),
		lifecycleLanes: make(map[lifecycleLaneKey]*lifecycleLane),
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
	if err := b.EmitSync(event, ctx); err != nil {
		return err
	}
	b.EmitAsync(event, ctx)
	return nil
}

// EmitSync runs only synchronous hooks and returns the first rejection.
func (b *EventBus) EmitSync(event EventType, ctx *EventContext) error {
	for _, h := range b.snapshot(event) {
		if h.Mode != HookSync {
			continue
		}
		if err := h.Handler(ctx); err != nil {
			return err
		}
	}
	return nil
}

// EmitAsync starts only asynchronous lifecycle hooks.
func (b *EventBus) EmitAsync(event EventType, ctx *EventContext) {
	hooks := asyncHooks(b.snapshot(event))
	if len(hooks) == 0 {
		return
	}
	ctx = cloneEventContext(ctx)
	if key, ok := eventLifecycleKey(event, ctx); ok {
		b.enqueueLifecycle(key, lifecycleDispatch{ctx: ctx, hooks: hooks})
		return
	}
	for _, hook := range hooks {
		go hook.Handler(ctx) //nolint:errcheck
	}
}

func asyncHooks(hooks []HookRegistration) []HookRegistration {
	async := make([]HookRegistration, 0, len(hooks))
	for _, hook := range hooks {
		if hook.Mode == HookAsync {
			async = append(async, hook)
		}
	}
	return async
}

func eventLifecycleKey(event EventType, ctx *EventContext) (lifecycleLaneKey, bool) {
	if ctx == nil || ctx.StreamKey == "" {
		return lifecycleLaneKey{}, false
	}
	switch event {
	case EventPublish, EventPublishStop:
		if ctx.PublisherID == "" {
			return lifecycleLaneKey{}, false
		}
		return lifecycleLaneKey{streamKey: ctx.StreamKey, clientID: ctx.PublisherID, publish: true}, true
	case EventSubscribe, EventSubscribeStop:
		if ctx.SubscriberID == "" {
			return lifecycleLaneKey{}, false
		}
		return lifecycleLaneKey{streamKey: ctx.StreamKey, clientID: ctx.SubscriberID}, true
	default:
		return lifecycleLaneKey{}, false
	}
}

func (b *EventBus) enqueueLifecycle(key lifecycleLaneKey, dispatch lifecycleDispatch) {
	b.asyncMu.Lock()
	lane := b.lifecycleLanes[key]
	if lane == nil {
		lane = &lifecycleLane{}
		b.lifecycleLanes[key] = lane
	}
	lane.queue = append(lane.queue, dispatch)
	startWorker := !lane.running
	if startWorker {
		lane.running = true
	}
	b.asyncMu.Unlock()
	if startWorker {
		go b.runLifecycleLane(key, lane)
	}
}

func (b *EventBus) runLifecycleLane(key lifecycleLaneKey, lane *lifecycleLane) {
	for {
		b.asyncMu.Lock()
		if len(lane.queue) == 0 {
			lane.running = false
			if b.lifecycleLanes[key] == lane {
				delete(b.lifecycleLanes, key)
			}
			b.asyncMu.Unlock()
			return
		}
		dispatch := lane.queue[0]
		lane.queue[0] = lifecycleDispatch{}
		lane.queue = lane.queue[1:]
		b.asyncMu.Unlock()

		for _, hook := range dispatch.hooks {
			_ = hook.Handler(dispatch.ctx)
		}
	}
}

func cloneEventContext(ctx *EventContext) *EventContext {
	if ctx == nil {
		return nil
	}
	clone := *ctx
	if ctx.Params != nil {
		clone.Params = make(map[string]string, len(ctx.Params))
		for key, value := range ctx.Params {
			clone.Params[key] = value
		}
	}
	if ctx.Extra != nil {
		clone.Extra = make(map[string]any, len(ctx.Extra))
		for key, value := range ctx.Extra {
			clone.Extra[key] = value
		}
	}
	return &clone
}

func (b *EventBus) snapshot(event EventType) []HookRegistration {
	b.mu.RLock()
	hooks := b.hooks[event]
	// Copy slice under lock to avoid races if Register is called concurrently
	copied := make([]HookRegistration, len(hooks))
	copy(copied, hooks)
	b.mu.RUnlock()
	return copied
}
