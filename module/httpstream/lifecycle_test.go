package httpstream

import (
	"context"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/im-pingo/liveforge/core"
)

type blockedSubscribeHooks struct {
	startEntered chan struct{}
	releaseStart chan struct{}
	stopRan      chan struct{}
	starts       chan *core.EventContext
	stops        chan *core.EventContext
	startOnce    sync.Once
	stopOnce     sync.Once
}

func registerBlockedSubscribeHooks(bus *core.EventBus) *blockedSubscribeHooks {
	hooks := &blockedSubscribeHooks{
		startEntered: make(chan struct{}),
		releaseStart: make(chan struct{}),
		stopRan:      make(chan struct{}),
		starts:       make(chan *core.EventContext, 1),
		stops:        make(chan *core.EventContext, 1),
	}
	bus.Register(core.HookRegistration{
		Event: core.EventSubscribe,
		Mode:  core.HookAsync,
		Handler: func(ctx *core.EventContext) error {
			hooks.starts <- ctx
			hooks.startOnce.Do(func() { close(hooks.startEntered) })
			<-hooks.releaseStart
			return nil
		},
	})
	bus.Register(core.HookRegistration{
		Event: core.EventSubscribeStop,
		Mode:  core.HookAsync,
		Handler: func(ctx *core.EventContext) error {
			hooks.stops <- ctx
			hooks.stopOnce.Do(func() { close(hooks.stopRan) })
			return nil
		},
	})
	return hooks
}

func (h *blockedSubscribeHooks) assertOrderedStableID(t *testing.T) {
	t.Helper()
	var start *core.EventContext
	select {
	case start = <-h.starts:
	case <-time.After(2 * time.Second):
		t.Fatal("subscribe start did not run")
	}

	stopOvertook := false
	select {
	case <-h.stopRan:
		stopOvertook = true
	case <-time.After(50 * time.Millisecond):
	}
	close(h.releaseStart)

	var stop *core.EventContext
	select {
	case stop = <-h.stops:
	case <-time.After(2 * time.Second):
		t.Fatal("subscribe stop did not run after releasing start")
	}
	if stopOvertook {
		t.Error("subscribe stop overtook the blocked start")
	}
	if start.SubscriberID == "" {
		t.Error("subscribe start has an empty subscriber ID")
	}
	if stop.SubscriberID != start.SubscriberID {
		t.Errorf("subscribe stop ID = %q, want start ID %q", stop.SubscriberID, start.SubscriberID)
	}
	if start.StreamInstanceID == 0 || start.PublisherGeneration == 0 || start.PublisherID == "" {
		t.Errorf("subscribe start omitted publisher identity: %+v", start)
	}
	if stop.StreamInstanceID != start.StreamInstanceID || stop.PublisherGeneration != start.PublisherGeneration || stop.PublisherID != start.PublisherID {
		t.Errorf("subscribe stop publisher identity = %+v, want %+v", stop, start)
	}
}

func TestHTTPSubscriberLifecycleSerializesBlockedStartBeforeStop(t *testing.T) {
	m, srv, addr := newHTTPTestServer(t)
	stream, err := srv.StreamHub().GetOrCreate("live/lifecycle")
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.SetPublisher(dummyPublisher{}); err != nil {
		t.Fatal(err)
	}
	m.registeredMu.Lock()
	m.registered[stream.Key()] = stream.InstanceID()
	m.registeredMu.Unlock()
	stream.MuxerManager().RegisterMuxerStart("ts", func(inst *core.MuxerInstance, _ *core.Stream) {
		inst.Buffer.Close()
	})
	hooks := registerBlockedSubscribeHooks(srv.GetEventBus())

	requestDone := make(chan error, 1)
	go func() {
		resp, requestErr := http.Get(addr + "/live/lifecycle.ts")
		if requestErr == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		requestDone <- requestErr
	}()

	select {
	case <-hooks.startEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("HTTP subscribe start did not enter")
	}
	select {
	case err := <-requestDone:
		if err != nil {
			t.Fatalf("HTTP request: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("HTTP stream did not finish")
	}
	hooks.assertOrderedStableID(t)
}

func TestWebSocketSubscriberLifecycleSerializesBlockedStartBeforeStop(t *testing.T) {
	m, srv, addr := newTestServer(t)
	stream, err := srv.StreamHub().GetOrCreate("live/lifecycle")
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.SetPublisher(dummyPublisher{}); err != nil {
		t.Fatal(err)
	}
	m.registeredMu.Lock()
	m.registered[stream.Key()] = stream.InstanceID()
	m.registeredMu.Unlock()
	stream.MuxerManager().RegisterMuxerStart("ts", func(inst *core.MuxerInstance, _ *core.Stream) {
		inst.Buffer.Close()
	})
	hooks := registerBlockedSubscribeHooks(srv.GetEventBus())

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, addr+"/ws/live/lifecycle.ts", nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("websocket dial: %v", err)
	}
	defer conn.CloseNow()
	select {
	case <-hooks.startEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("WebSocket subscribe start did not enter")
	}
	_ = conn.Close(websocket.StatusNormalClosure, "done")
	hooks.assertOrderedStableID(t)
}
