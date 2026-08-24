package core

import (
	"errors"
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
