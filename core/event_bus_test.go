package core

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestEventBusReservesTerminalOnlyConsumerLaneOnStart(t *testing.T) {
	bus := NewEventBus()
	started := make(chan struct{}, 1)
	recordStopped := make(chan struct{}, 1)
	httpStopped := make(chan struct{}, 1)
	bus.Register(HookRegistration{Event: EventPublish, Mode: HookAsync, Consumer: "record", Handler: func(*EventContext) error {
		started <- struct{}{}
		return nil
	}})
	bus.Register(HookRegistration{Event: EventPublishStop, Mode: HookAsync, Consumer: "record", Handler: func(*EventContext) error {
		recordStopped <- struct{}{}
		return nil
	}})
	bus.Register(HookRegistration{Event: EventPublishStop, Mode: HookAsync, Consumer: "httpstream", Handler: func(*EventContext) error {
		httpStopped <- struct{}{}
		return nil
	}})
	ctx := &EventContext{StreamKey: "live/terminal-reservation", PublisherID: "publisher-1"}

	if err := bus.EmitAsync(EventPublish, ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("publish start hook did not run")
	}

	bus.asyncMu.Lock()
	reserved := len(bus.lifecycleLanes)
	bus.asyncMu.Unlock()
	if reserved != 2 {
		t.Fatalf("reserved lifecycle lanes = %d, want start and terminal-only consumers", reserved)
	}

	if err := bus.EmitAsync(EventPublishStop, ctx); err != nil {
		t.Fatal(err)
	}
	for name, done := range map[string]<-chan struct{}{"record": recordStopped, "httpstream": httpStopped} {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("%s terminal hook did not run", name)
		}
	}
	waitEventBusLanesReleased(t, bus)
}

func TestEventBusReservesTerminalOnlyLaneWithoutStartHooks(t *testing.T) {
	bus := NewEventBus()
	stopped := make(chan struct{}, 1)
	bus.Register(HookRegistration{Event: EventPublishStop, Mode: HookAsync, Consumer: "record", Handler: func(*EventContext) error {
		stopped <- struct{}{}
		return nil
	}})
	ctx := &EventContext{StreamKey: "live/terminal-only", PublisherID: "publisher-1"}

	if err := bus.EmitAsync(EventPublish, ctx); err != nil {
		t.Fatal(err)
	}
	bus.asyncMu.Lock()
	reserved := len(bus.lifecycleLanes)
	bus.asyncMu.Unlock()
	if reserved != 1 {
		t.Fatalf("reserved lifecycle lanes = %d, want terminal-only consumer", reserved)
	}

	if err := bus.EmitAsync(EventPublishStop, ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("terminal-only hook did not run")
	}
	waitEventBusLanesReleased(t, bus)
}

func TestEventBusReservesEveryTerminalHookSlotForConsumer(t *testing.T) {
	bus := NewEventBus()
	entered := make(chan struct{})
	release := make(chan struct{})
	var starts atomic.Int32
	var stops atomic.Int32
	bus.Register(HookRegistration{Event: EventPublish, Mode: HookAsync, Consumer: "record", Handler: func(*EventContext) error {
		if starts.Add(1) == 1 {
			close(entered)
			<-release
		}
		return nil
	}})
	for range 2 {
		bus.Register(HookRegistration{Event: EventPublishStop, Mode: HookAsync, Consumer: "record", Handler: func(*EventContext) error {
			stops.Add(1)
			return nil
		}})
	}
	ctx := &EventContext{StreamKey: "live/terminal-slots", PublisherID: "publisher-1"}
	if err := bus.EmitAsync(EventPublish, ctx); err != nil {
		t.Fatal(err)
	}
	<-entered
	for i := 0; i < maxLifecycleQueueDepth-2; i++ {
		if err := bus.EmitAsync(EventPublish, ctx); err != nil {
			t.Fatalf("start queue admission %d: %v", i, err)
		}
	}
	if err := bus.EmitAsync(EventPublish, ctx); !errors.Is(err, ErrAsyncBackpressure) {
		t.Fatalf("start consumed terminal reservation: %v", err)
	}
	if err := bus.EmitAsync(EventPublishStop, ctx); err != nil {
		t.Fatalf("terminal hooks did not fit reserved slots: %v", err)
	}
	close(release)
	waitEventBusLanesReleased(t, bus)
	if got := stops.Load(); got != 2 {
		t.Fatalf("terminal hook calls = %d, want 2", got)
	}
}

func TestEventBusDrainWaitsForAsyncHooksAndHonorsContext(t *testing.T) {
	bus := NewEventBus()
	entered := make(chan struct{})
	release := make(chan struct{})
	bus.Register(HookRegistration{Event: EventStreamAlive, Mode: HookAsync, Handler: func(*EventContext) error {
		close(entered)
		<-release
		return nil
	}})
	if err := bus.EmitAsync(EventStreamAlive, &EventContext{StreamKey: "live/drain"}); err != nil {
		t.Fatal(err)
	}
	<-entered

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := bus.Drain(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Drain() error = %v, want context deadline", err)
	}

	close(release)
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := bus.Drain(ctx); err != nil {
		t.Fatalf("Drain() after release: %v", err)
	}
}

func TestEventBusDrainCompletesAfterPanickingHook(t *testing.T) {
	bus := NewEventBus()
	bus.Register(HookRegistration{Event: EventStreamAlive, Mode: HookAsync, Handler: func(*EventContext) error {
		panic("expected test panic")
	}})
	if err := bus.EmitAsync(EventStreamAlive, &EventContext{StreamKey: "live/panic-drain"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := bus.Drain(ctx); err != nil {
		t.Fatalf("Drain() after panic: %v", err)
	}
}

func TestEventBusSyncHook(t *testing.T) {
	bus := NewEventBus()
	var called int32

	bus.Register(HookRegistration{
		Event:    EventPublish,
		Mode:     HookSync,
		Priority: 10,
		Handler: func(ctx *EventContext) error {
			atomic.AddInt32(&called, 1)
			return nil
		},
	})

	err := bus.Emit(EventPublish, &EventContext{StreamKey: "live/test"})
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if atomic.LoadInt32(&called) != 1 {
		t.Errorf("expected handler called once, got %d", called)
	}
}

func TestEventBusSerializesPublishLifecyclePerGeneration(t *testing.T) {
	bus := NewEventBus()
	startEntered := make(chan struct{})
	releaseStart := make(chan struct{})
	done := make(chan struct{})
	var (
		mu    sync.Mutex
		order []EventType
	)
	record := func(event EventType, block bool) EventHandler {
		return func(*EventContext) error {
			if block {
				close(startEntered)
				<-releaseStart
			}
			mu.Lock()
			order = append(order, event)
			if len(order) == 2 {
				close(done)
			}
			mu.Unlock()
			return nil
		}
	}
	bus.Register(HookRegistration{Event: EventPublish, Mode: HookAsync, Handler: record(EventPublish, true)})
	bus.Register(HookRegistration{Event: EventPublishStop, Mode: HookAsync, Handler: record(EventPublishStop, false)})
	ctx := &EventContext{StreamKey: "live/ordered", PublisherID: "publisher-1"}

	bus.EmitAsync(EventPublish, ctx)
	select {
	case <-startEntered:
	case <-time.After(time.Second):
		t.Fatal("publish start handler did not block")
	}
	bus.EmitAsync(EventPublishStop, ctx)
	select {
	case <-done:
		t.Fatal("publish stop overtook blocked publish start")
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseStart)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("serialized lifecycle did not drain")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[0] != EventPublish || order[1] != EventPublishStop {
		t.Fatalf("lifecycle order = %v, want publish then publish-stop", order)
	}
}

func TestEventBusLifecycleConsumersDoNotBlockEachOther(t *testing.T) {
	bus := NewEventBus()
	blockedStart := make(chan struct{})
	releaseBlocked := make(chan struct{})
	blockedStop := make(chan struct{})
	independentOrder := make(chan EventType, 2)

	bus.Register(HookRegistration{Event: EventPublish, Mode: HookAsync, Priority: 10, Consumer: "blocked", Handler: func(*EventContext) error {
		close(blockedStart)
		<-releaseBlocked
		return nil
	}})
	bus.Register(HookRegistration{Event: EventPublishStop, Mode: HookAsync, Priority: 10, Consumer: "blocked", Handler: func(*EventContext) error {
		close(blockedStop)
		return nil
	}})
	bus.Register(HookRegistration{Event: EventPublish, Mode: HookAsync, Priority: 20, Consumer: "independent", Handler: func(*EventContext) error {
		independentOrder <- EventPublish
		return nil
	}})
	bus.Register(HookRegistration{Event: EventPublishStop, Mode: HookAsync, Priority: 20, Consumer: "independent", Handler: func(*EventContext) error {
		independentOrder <- EventPublishStop
		return nil
	}})

	ctx := &EventContext{StreamKey: "live/consumers", PublisherID: "publisher-1"}
	bus.EmitAsync(EventPublish, ctx)
	select {
	case <-blockedStart:
	case <-time.After(time.Second):
		t.Fatal("blocked consumer did not enter start")
	}
	select {
	case event := <-independentOrder:
		if event != EventPublish {
			t.Fatalf("independent first event = %v, want publish", event)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked consumer starved independent start")
	}

	bus.EmitAsync(EventPublishStop, ctx)
	select {
	case event := <-independentOrder:
		if event != EventPublishStop {
			t.Fatalf("independent second event = %v, want publish-stop", event)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked consumer starved independent stop")
	}
	select {
	case <-blockedStop:
		t.Fatal("blocked consumer stop overtook its own start")
	case <-time.After(20 * time.Millisecond):
	}

	close(releaseBlocked)
	select {
	case <-blockedStop:
	case <-time.After(time.Second):
		t.Fatal("blocked consumer did not drain after start released")
	}
}

func TestEventBusLifecycleQueueBackpressureIsBoundedAndObservable(t *testing.T) {
	bus := NewEventBus()
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	bus.Register(HookRegistration{Event: EventPublish, Mode: HookAsync, Consumer: "blocked", Handler: func(*EventContext) error {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		return nil
	}})
	bus.Register(HookRegistration{Event: EventPublishStop, Mode: HookAsync, Consumer: "blocked", Handler: func(*EventContext) error {
		return nil
	}})
	ctx := &EventContext{StreamKey: "live/saturated", PublisherID: "publisher-1"}
	if err := bus.EmitAsync(EventPublish, ctx); err != nil {
		t.Fatalf("initial lifecycle admission: %v", err)
	}
	<-entered
	for i := 0; i < maxLifecycleQueueDepth-1; i++ {
		if err := bus.EmitAsync(EventPublish, ctx); err != nil {
			t.Fatalf("queue admission %d: %v", i, err)
		}
	}
	if err := bus.EmitAsync(EventPublish, ctx); !errors.Is(err, ErrAsyncBackpressure) {
		t.Fatalf("saturated admission error = %v, want %v", err, ErrAsyncBackpressure)
	}
	if err := bus.EmitAsync(EventPublishStop, ctx); err != nil {
		t.Fatalf("terminal lifecycle event did not use its reserved queue slot: %v", err)
	}
	if got := bus.AsyncStats().Rejected; got != 1 {
		t.Fatalf("rejected dispatches = %d, want 1", got)
	}

	close(release)
	waitEventBusLanesReleased(t, bus)
	if got := calls.Load(); got != int32(maxLifecycleQueueDepth) {
		t.Fatalf("accepted publish calls = %d, want %d", got, maxLifecycleQueueDepth)
	}
}

func TestEventBusRetainsIdleLifecycleLaneUntilTerminalEvent(t *testing.T) {
	bus := NewEventBus()
	started := make(chan struct{})
	stopped := make(chan struct{})
	bus.Register(HookRegistration{Event: EventPublish, Mode: HookAsync, Consumer: "record", Handler: func(*EventContext) error {
		close(started)
		return nil
	}})
	bus.Register(HookRegistration{Event: EventPublishStop, Mode: HookAsync, Consumer: "record", Handler: func(*EventContext) error {
		close(stopped)
		return nil
	}})
	ctx := &EventContext{StreamKey: "live/idle-lane", PublisherID: "publisher-1"}
	if err := bus.EmitAsync(EventPublish, ctx); err != nil {
		t.Fatal(err)
	}
	<-started

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		bus.asyncMu.Lock()
		laneCount := len(bus.lifecycleLanes)
		var running bool
		for _, lane := range bus.lifecycleLanes {
			running = lane.running
		}
		bus.asyncMu.Unlock()
		if laneCount == 1 && !running {
			break
		}
		time.Sleep(time.Millisecond)
	}
	bus.asyncMu.Lock()
	retained := len(bus.lifecycleLanes) == 1
	bus.asyncMu.Unlock()
	if !retained {
		t.Fatal("lifecycle lane was released before its terminal event")
	}

	if err := bus.EmitAsync(EventPublishStop, ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("terminal lifecycle event did not run")
	}
	waitEventBusLanesReleased(t, bus)
}

func TestEventBusReleasesStartOnlyConsumerLane(t *testing.T) {
	bus := NewEventBus()
	done := make(chan struct{})
	bus.Register(HookRegistration{Event: EventSubscribe, Mode: HookAsync, Consumer: "origin-pull", Handler: func(*EventContext) error {
		close(done)
		return nil
	}})
	if err := bus.EmitAsync(EventSubscribe, &EventContext{StreamKey: "live/pull", SubscriberID: "subscriber-1"}); err != nil {
		t.Fatal(err)
	}
	<-done
	waitEventBusLanesReleased(t, bus)
}

func TestEventBusLifecycleWorkerRecoversPanicAndReleasesLane(t *testing.T) {
	bus := NewEventBus()
	stopRan := make(chan struct{})
	bus.Register(HookRegistration{Event: EventPublish, Mode: HookAsync, Consumer: "panic", Handler: func(*EventContext) error {
		panic("test panic")
	}})
	bus.Register(HookRegistration{Event: EventPublishStop, Mode: HookAsync, Consumer: "panic", Handler: func(*EventContext) error {
		close(stopRan)
		return nil
	}})
	ctx := &EventContext{StreamKey: "live/panic", PublisherID: "publisher-1"}
	if err := bus.EmitAsync(EventPublish, ctx); err != nil {
		t.Fatal(err)
	}
	if err := bus.EmitAsync(EventPublishStop, ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopRan:
	case <-time.After(time.Second):
		t.Fatal("panic prevented lifecycle stop from draining")
	}
	waitEventBusLanesReleased(t, bus)
}

func waitEventBusLanesReleased(t *testing.T, bus *EventBus) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		bus.asyncMu.Lock()
		remaining := len(bus.lifecycleLanes)
		bus.asyncMu.Unlock()
		if remaining == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("completed lifecycle lanes leaked dispatcher state")
}

func TestEventBusLifecycleGenerationsRunIndependentlyAndReleaseState(t *testing.T) {
	bus := NewEventBus()
	blocked := make(chan struct{})
	release := make(chan struct{})
	secondRan := make(chan struct{})
	firstDone := make(chan struct{})
	bus.Register(HookRegistration{
		Event: EventPublish,
		Mode:  HookAsync,
		Handler: func(ctx *EventContext) error {
			switch ctx.PublisherID {
			case "publisher-1":
				close(blocked)
				<-release
			case "publisher-2":
				close(secondRan)
			}
			return nil
		},
	})
	bus.Register(HookRegistration{
		Event: EventPublishStop,
		Mode:  HookAsync,
		Handler: func(ctx *EventContext) error {
			if ctx.PublisherID == "publisher-1" {
				close(firstDone)
			}
			return nil
		},
	})

	if err := bus.EmitAsync(EventPublish, &EventContext{StreamKey: "live/generations", PublisherID: "publisher-1"}); err != nil {
		t.Fatal(err)
	}
	<-blocked
	if err := bus.EmitAsync(EventPublishStop, &EventContext{StreamKey: "live/generations", PublisherID: "publisher-1"}); err != nil {
		t.Fatal(err)
	}
	if err := bus.EmitAsync(EventPublish, &EventContext{StreamKey: "live/generations", PublisherID: "publisher-2"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-secondRan:
	case <-time.After(time.Second):
		t.Fatal("independent publisher generation was blocked")
	}
	if err := bus.EmitAsync(EventPublishStop, &EventContext{StreamKey: "live/generations", PublisherID: "publisher-2"}); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("first publisher generation did not finish")
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		bus.asyncMu.Lock()
		remaining := len(bus.lifecycleLanes)
		bus.asyncMu.Unlock()
		if remaining == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("completed lifecycle generations leaked dispatcher state")
}

func TestEventBusSerializesSubscribeLifecycleBySubscriber(t *testing.T) {
	bus := NewEventBus()
	startEntered := make(chan struct{})
	releaseStart := make(chan struct{})
	stopRan := make(chan struct{})
	bus.Register(HookRegistration{
		Event: EventSubscribe,
		Mode:  HookAsync,
		Handler: func(*EventContext) error {
			close(startEntered)
			<-releaseStart
			return nil
		},
	})
	bus.Register(HookRegistration{
		Event: EventSubscribeStop,
		Mode:  HookAsync,
		Handler: func(*EventContext) error {
			close(stopRan)
			return nil
		},
	})
	ctx := &EventContext{StreamKey: "live/subscribe", SubscriberID: "subscriber-1"}
	bus.EmitAsync(EventSubscribe, ctx)
	<-startEntered
	bus.EmitAsync(EventSubscribeStop, ctx)
	select {
	case <-stopRan:
		t.Fatal("subscribe stop overtook blocked subscribe start")
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseStart)
	select {
	case <-stopRan:
	case <-time.After(time.Second):
		t.Fatal("subscribe lifecycle did not drain")
	}
}

func TestEventBusSyncHookReject(t *testing.T) {
	bus := NewEventBus()
	errAuth := errors.New("unauthorized")

	bus.Register(HookRegistration{
		Event:    EventPublish,
		Mode:     HookSync,
		Priority: 10,
		Handler: func(ctx *EventContext) error {
			return errAuth
		},
	})

	err := bus.Emit(EventPublish, &EventContext{StreamKey: "live/test"})
	if !errors.Is(err, errAuth) {
		t.Errorf("expected errAuth, got %v", err)
	}
}

func TestEventBusPriorityOrder(t *testing.T) {
	bus := NewEventBus()
	var order []int

	bus.Register(HookRegistration{
		Event: EventPublish, Mode: HookSync, Priority: 20,
		Handler: func(ctx *EventContext) error { order = append(order, 20); return nil },
	})
	bus.Register(HookRegistration{
		Event: EventPublish, Mode: HookSync, Priority: 10,
		Handler: func(ctx *EventContext) error { order = append(order, 10); return nil },
	})
	bus.Register(HookRegistration{
		Event: EventPublish, Mode: HookSync, Priority: 15,
		Handler: func(ctx *EventContext) error { order = append(order, 15); return nil },
	})

	_ = bus.Emit(EventPublish, &EventContext{})
	if len(order) != 3 || order[0] != 10 || order[1] != 15 || order[2] != 20 {
		t.Errorf("expected priority order [10,15,20], got %v", order)
	}
}

func TestEventBusAsyncHook(t *testing.T) {
	bus := NewEventBus()
	done := make(chan struct{})

	bus.Register(HookRegistration{
		Event: EventPublish, Mode: HookAsync, Priority: 90,
		Handler: func(ctx *EventContext) error {
			close(done)
			return nil
		},
	})

	err := bus.Emit(EventPublish, &EventContext{})
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	select {
	case <-done:
		// OK
	case <-time.After(1 * time.Second):
		t.Fatal("async handler was not called within 1s")
	}
}

func TestEventBusSeparatesSyncAuthorizationFromAsyncLifecycle(t *testing.T) {
	bus := NewEventBus()
	actionStarted := false
	asyncObserved := make(chan bool, 1)
	bus.Register(HookRegistration{
		Event: EventPublish,
		Mode:  HookSync,
		Handler: func(*EventContext) error {
			if actionStarted {
				t.Fatal("sync authorization ran after publish action")
			}
			return nil
		},
	})
	bus.Register(HookRegistration{
		Event: EventPublish,
		Mode:  HookAsync,
		Handler: func(*EventContext) error {
			asyncObserved <- actionStarted
			return nil
		},
	})

	if err := bus.EmitSync(EventPublish, &EventContext{StreamKey: "live/two-phase"}); err != nil {
		t.Fatal(err)
	}
	actionStarted = true
	bus.EmitAsync(EventPublish, &EventContext{StreamKey: "live/two-phase"})
	select {
	case observed := <-asyncObserved:
		if !observed {
			t.Fatal("async lifecycle ran before publish action")
		}
	case <-time.After(time.Second):
		t.Fatal("async lifecycle hook did not run")
	}
}
