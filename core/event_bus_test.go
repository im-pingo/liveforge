package core

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

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

	bus.EmitAsync(EventPublish, &EventContext{StreamKey: "live/generations", PublisherID: "publisher-1"})
	<-blocked
	bus.EmitAsync(EventPublishStop, &EventContext{StreamKey: "live/generations", PublisherID: "publisher-1"})
	bus.EmitAsync(EventPublish, &EventContext{StreamKey: "live/generations", PublisherID: "publisher-2"})
	select {
	case <-secondRan:
	case <-time.After(time.Second):
		t.Fatal("independent publisher generation was blocked")
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
