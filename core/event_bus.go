package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
)

const (
	maxLifecycleQueueDepth = 8
	maxLifecycleLanes      = 4096
)

var ErrAsyncBackpressure = errors.New("event bus async lifecycle capacity exceeded")

type AsyncDispatchStats struct {
	Rejected uint64
}

// EventBus dispatches events to registered hook handlers.
type EventBus struct {
	mu    sync.RWMutex
	hooks map[EventType][]HookRegistration

	asyncMu        sync.Mutex
	lifecycleLanes map[lifecycleLaneKey]*lifecycleLane
	autoConsumers  map[autoConsumerKey]uint64
	asyncRejected  atomic.Uint64
	dispatchMu     sync.Mutex
	pendingAsync   int
	asyncIdle      chan struct{}
}

type lifecycleLaneKey struct {
	streamKey string
	clientID  string
	publish   bool
	consumer  string
}

type lifecycleDispatch struct {
	ctx      *EventContext
	hook     HookRegistration
	terminal bool
}

type lifecycleLane struct {
	queue          []lifecycleDispatch
	running        bool
	closeWhenEmpty bool
}

type autoConsumerKey struct {
	event    EventType
	priority int
}

// NewEventBus creates a new EventBus.
func NewEventBus() *EventBus {
	idle := make(chan struct{})
	close(idle)
	return &EventBus{
		hooks:          make(map[EventType][]HookRegistration),
		lifecycleLanes: make(map[lifecycleLaneKey]*lifecycleLane),
		autoConsumers:  make(map[autoConsumerKey]uint64),
		asyncIdle:      idle,
	}
}

// Register adds a hook registration, maintaining priority order.
func (b *EventBus) Register(h HookRegistration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if h.Consumer == "" && h.Mode == HookAsync {
		if family, ok := lifecycleFamily(h.Event); ok {
			key := autoConsumerKey{event: h.Event, priority: h.Priority}
			ordinal := b.autoConsumers[key]
			b.autoConsumers[key] = ordinal + 1
			h.Consumer = fmt.Sprintf("auto:%d:%d:%d", family, h.Priority, ordinal)
		}
	}

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
	return b.EmitAsync(event, ctx)
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

// EmitAsync starts only asynchronous hooks. Lifecycle admission is bounded
// and atomic across consumers; callers receive ErrAsyncBackpressure rather
// than a partial start/stop delivery when capacity is exhausted.
func (b *EventBus) EmitAsync(event EventType, ctx *EventContext) error {
	hooks := asyncHooks(b.snapshot(event))
	if key, ok := eventLifecycleKey(event, ctx); ok {
		terminalCounts := b.terminalConsumerCounts(event)
		if len(hooks) > 0 || len(terminalCounts) > 0 {
			return b.enqueueLifecycle(event, key, hooks, ctx, terminalCounts)
		}
		return nil
	}
	if len(hooks) == 0 {
		return nil
	}
	b.beginAsync(len(hooks))
	for _, hook := range hooks {
		go b.runTrackedAsyncHook(hook, cloneEventContext(ctx))
	}
	return nil
}

// Drain waits for all asynchronous hook dispatches accepted before the bus
// becomes idle. Callers must stop event producers before relying on an idle
// result as a shutdown barrier.
func (b *EventBus) Drain(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	b.dispatchMu.Lock()
	idle := b.asyncIdle
	b.dispatchMu.Unlock()
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *EventBus) beginAsync(count int) {
	if count <= 0 {
		return
	}
	b.dispatchMu.Lock()
	if b.pendingAsync == 0 {
		b.asyncIdle = make(chan struct{})
	}
	b.pendingAsync += count
	b.dispatchMu.Unlock()
}

func (b *EventBus) completeAsync() {
	b.dispatchMu.Lock()
	b.pendingAsync--
	if b.pendingAsync == 0 {
		close(b.asyncIdle)
	}
	b.dispatchMu.Unlock()
}

func (b *EventBus) runTrackedAsyncHook(hook HookRegistration, ctx *EventContext) {
	defer b.completeAsync()
	runAsyncHook(hook, ctx)
}

func (b *EventBus) AsyncStats() AsyncDispatchStats {
	return AsyncDispatchStats{Rejected: b.asyncRejected.Load()}
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

func lifecycleFamily(event EventType) (uint8, bool) {
	switch event {
	case EventPublish, EventPublishStop:
		return 1, true
	case EventSubscribe, EventSubscribeStop:
		return 2, true
	default:
		return 0, false
	}
}

func (b *EventBus) terminalConsumerCounts(event EventType) map[string]int {
	var terminal EventType
	switch event {
	case EventPublish:
		terminal = EventPublishStop
	case EventSubscribe:
		terminal = EventSubscribeStop
	default:
		return nil
	}
	consumers := make(map[string]int)
	for _, hook := range asyncHooks(b.snapshot(terminal)) {
		consumers[hook.Consumer]++
	}
	return consumers
}

func (b *EventBus) enqueueLifecycle(event EventType, base lifecycleLaneKey, hooks []HookRegistration, ctx *EventContext, terminalCounts map[string]int) error {
	type laneStart struct {
		key  lifecycleLaneKey
		lane *lifecycleLane
	}
	counts := make(map[lifecycleLaneKey]int, len(hooks))
	holdOpen := make(map[lifecycleLaneKey]bool, len(hooks))
	terminal := event == EventPublishStop || event == EventSubscribeStop
	for _, hook := range hooks {
		key := base
		key.consumer = hook.Consumer
		counts[key]++
		holdOpen[key] = terminalCounts[hook.Consumer] > 0
	}
	if !terminal {
		for consumer := range terminalCounts {
			key := base
			key.consumer = consumer
			if _, exists := counts[key]; !exists {
				counts[key] = 0
			}
			holdOpen[key] = true
		}
	}

	b.asyncMu.Lock()
	newLanes := 0
	for key, count := range counts {
		lane := b.lifecycleLanes[key]
		limit := maxLifecycleQueueDepth
		if !terminal {
			limit -= terminalCounts[key.consumer]
		}
		if lane == nil {
			if count > limit {
				b.asyncMu.Unlock()
				b.asyncRejected.Add(1)
				return ErrAsyncBackpressure
			}
			newLanes++
			continue
		}
		if len(lane.queue)+count > limit {
			b.asyncMu.Unlock()
			b.asyncRejected.Add(1)
			return ErrAsyncBackpressure
		}
	}
	if len(b.lifecycleLanes)+newLanes > maxLifecycleLanes {
		b.asyncMu.Unlock()
		b.asyncRejected.Add(1)
		return ErrAsyncBackpressure
	}
	starts := make([]laneStart, 0, newLanes)
	for key, count := range counts {
		lane := b.lifecycleLanes[key]
		if lane == nil {
			lane = &lifecycleLane{running: count > 0, closeWhenEmpty: terminal || !holdOpen[key]}
			b.lifecycleLanes[key] = lane
			if lane.running {
				starts = append(starts, laneStart{key: key, lane: lane})
			}
		} else if count > 0 && !lane.running {
			lane.running = true
			starts = append(starts, laneStart{key: key, lane: lane})
		}
		if !terminal && holdOpen[key] {
			lane.closeWhenEmpty = false
		}
	}
	for _, hook := range hooks {
		key := base
		key.consumer = hook.Consumer
		lane := b.lifecycleLanes[key]
		lane.queue = append(lane.queue, lifecycleDispatch{ctx: cloneEventContext(ctx), hook: hook, terminal: terminal})
	}
	b.beginAsync(len(hooks))
	b.asyncMu.Unlock()
	for _, start := range starts {
		go b.runLifecycleLane(start.key, start.lane)
	}
	return nil
}

func (b *EventBus) runLifecycleLane(key lifecycleLaneKey, lane *lifecycleLane) {
	for {
		b.asyncMu.Lock()
		if len(lane.queue) == 0 {
			lane.running = false
			if lane.closeWhenEmpty && b.lifecycleLanes[key] == lane {
				delete(b.lifecycleLanes, key)
			}
			b.asyncMu.Unlock()
			return
		}
		dispatch := lane.queue[0]
		lane.queue[0] = lifecycleDispatch{}
		lane.queue = lane.queue[1:]
		b.asyncMu.Unlock()

		b.runTrackedAsyncHook(dispatch.hook, dispatch.ctx)
		if dispatch.terminal {
			b.asyncMu.Lock()
			lane.closeWhenEmpty = true
			b.asyncMu.Unlock()
		}
	}
}

func runAsyncHook(hook HookRegistration, ctx *EventContext) {
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Error("async event hook panicked", "event", hook.Event, "consumer", hook.Consumer, "panic", recovered)
		}
	}()
	_ = hook.Handler(ctx)
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
