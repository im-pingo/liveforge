package rtmp

import (
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
)

func TestRTMPPublisherAdmissionFailureRollsBackPublisher(t *testing.T) {
	hub, bus := newTestHub()
	streamKey := "live/admission"
	publisherID := fmt.Sprintf("rtmp-pub-%s-%d", streamKey, publisherSequence.Load()+1)
	release := saturateRTMPPublishLifecycle(t, bus, &core.EventContext{StreamKey: streamKey, PublisherID: publisherID})
	defer release()

	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	go func() { _, _ = io.Copy(io.Discard, clientConn) }()
	handler := NewHandler(serverConn, hub, bus, 4096, nil)
	handler.app = "live"
	if err := handler.onPublish([]any{"publish", float64(0), nil, "admission"}); !errors.Is(err, core.ErrAsyncBackpressure) {
		t.Fatalf("onPublish() error = %v, want EventBus backpressure", err)
	}
	if handler.isPublisher || handler.publisher != nil {
		t.Fatal("handler retained publisher state after lifecycle admission failure")
	}
	if stream, ok := hub.Find(streamKey); ok && stream.Publisher() != nil {
		t.Fatal("stream retained publisher after lifecycle admission failure")
	}
}

func TestRTMPPublisherCleanupEmitsStopOnceAfterStreamRemovedFromHub(t *testing.T) {
	hub, bus := newTestHub()
	starts := make(chan *core.EventContext, 1)
	stops := make(chan *core.EventContext, 2)
	bus.Register(core.HookRegistration{Event: core.EventPublish, Mode: core.HookAsync, Handler: func(ctx *core.EventContext) error {
		starts <- ctx
		return nil
	}})
	bus.Register(core.HookRegistration{Event: core.EventPublishStop, Mode: core.HookAsync, Handler: func(ctx *core.EventContext) error {
		stops <- ctx
		return nil
	}})

	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	go func() { _, _ = io.Copy(io.Discard, clientConn) }()
	handler := NewHandler(serverConn, hub, bus, 4096, nil)
	handler.app = "live"
	if err := handler.onPublish([]any{"publish", float64(0), nil, "removed"}); err != nil {
		t.Fatal(err)
	}
	var start *core.EventContext
	select {
	case start = <-starts:
	case <-time.After(2 * time.Second):
		t.Fatal("RTMP publish start did not run")
	}

	hub.Remove("live/removed")
	handler.cleanup()
	handler.cleanup()
	var stop *core.EventContext
	select {
	case stop = <-stops:
	case <-time.After(2 * time.Second):
		t.Fatal("RTMP publish stop did not run after Hub removal")
	}
	if stop.StreamInstanceID != start.StreamInstanceID || stop.PublisherGeneration != start.PublisherGeneration || stop.PublisherID != start.PublisherID {
		t.Fatalf("RTMP stop identity = %+v, want start identity %+v", stop, start)
	}
	select {
	case duplicate := <-stops:
		t.Fatalf("duplicate RTMP publish stop: %+v", duplicate)
	case <-time.After(50 * time.Millisecond):
	}
}

func saturateRTMPPublishLifecycle(t *testing.T, bus *core.EventBus, ctx *core.EventContext) func() {
	t.Helper()
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	bus.Register(core.HookRegistration{Event: core.EventPublish, Mode: core.HookAsync, Consumer: "rtmp-admission-test", Handler: func(*core.EventContext) error {
		once.Do(func() { close(entered) })
		<-release
		return nil
	}})
	bus.Register(core.HookRegistration{Event: core.EventPublishStop, Mode: core.HookAsync, Consumer: "rtmp-admission-test", Handler: func(*core.EventContext) error { return nil }})
	if err := bus.EmitAsync(core.EventPublish, ctx); err != nil {
		t.Fatal(err)
	}
	<-entered
	for attempts := 0; ; attempts++ {
		err := bus.EmitAsync(core.EventPublish, ctx)
		if errors.Is(err, core.ErrAsyncBackpressure) {
			break
		}
		if err != nil || attempts > 32 {
			t.Fatalf("saturate lifecycle lane: attempts=%d error=%v", attempts, err)
		}
	}
	var releaseOnce sync.Once
	return func() { releaseOnce.Do(func() { close(release) }) }
}

type lifecyclePublisher struct{}

func (lifecyclePublisher) ID() string { return "source" }
func (lifecyclePublisher) MediaInfo() *avframe.MediaInfo {
	return &avframe.MediaInfo{VideoCodec: avframe.CodecH264}
}
func (lifecyclePublisher) Close() error { return nil }

func TestRTMPSubscriberLifecycleSerializesBlockedStartBeforeStop(t *testing.T) {
	hub, bus := newTestHub()
	stream, err := hub.GetOrCreate("live/lifecycle")
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.SetPublisher(lifecyclePublisher{}); err != nil {
		t.Fatal(err)
	}
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo,
		avframe.CodecH264,
		avframe.FrameTypeSequenceHeader,
		0,
		0,
		[]byte{0x01, 0x64},
	))

	startEntered := make(chan struct{})
	releaseStart := make(chan struct{})
	stopRan := make(chan struct{})
	starts := make(chan *core.EventContext, 1)
	stops := make(chan *core.EventContext, 1)
	var startOnce sync.Once
	var stopOnce sync.Once
	var authorizations atomic.Int32
	bus.Register(core.HookRegistration{Event: core.EventSubscribe, Mode: core.HookSync, Handler: func(*core.EventContext) error {
		authorizations.Add(1)
		return nil
	}})
	bus.Register(core.HookRegistration{Event: core.EventSubscribe, Mode: core.HookAsync, Handler: func(ctx *core.EventContext) error {
		starts <- ctx
		startOnce.Do(func() { close(startEntered) })
		<-releaseStart
		return nil
	}})
	bus.Register(core.HookRegistration{Event: core.EventSubscribeStop, Mode: core.HookAsync, Handler: func(ctx *core.EventContext) error {
		stops <- ctx
		stopOnce.Do(func() { close(stopRan) })
		return nil
	}})

	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	handler := NewHandler(serverConn, hub, bus, 4096, nil)
	handler.app = "live"
	go func() { _, _ = io.Copy(io.Discard, clientConn) }()
	if err := handler.onPlay([]any{"play", float64(0), nil, "lifecycle"}); err != nil {
		t.Fatalf("onPlay: %v", err)
	}

	var start *core.EventContext
	select {
	case start = <-starts:
	case <-time.After(2 * time.Second):
		t.Fatal("RTMP subscribe start did not run")
	}
	stream.Close()
	stopOvertook := false
	select {
	case <-stopRan:
		stopOvertook = true
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseStart)
	var stop *core.EventContext
	select {
	case stop = <-stops:
	case <-time.After(2 * time.Second):
		t.Fatal("RTMP subscribe stop did not run")
	}
	if stopOvertook {
		t.Error("RTMP subscribe stop overtook the blocked start")
	}
	if start.SubscriberID == "" {
		t.Error("RTMP subscribe start has an empty subscriber ID")
	}
	if stop.SubscriberID != start.SubscriberID {
		t.Errorf("RTMP subscribe stop ID = %q, want %q", stop.SubscriberID, start.SubscriberID)
	}
	if start.StreamInstanceID == 0 || start.PublisherGeneration == 0 || start.PublisherID == "" {
		t.Errorf("RTMP subscribe start omitted publisher identity: %+v", start)
	}
	if stop.StreamInstanceID != start.StreamInstanceID || stop.PublisherGeneration != start.PublisherGeneration || stop.PublisherID != start.PublisherID {
		t.Errorf("RTMP subscribe stop publisher identity = %+v, want %+v", stop, start)
	}
	if got := authorizations.Load(); got != 1 {
		t.Errorf("RTMP authorization calls = %d, want 1", got)
	}
}

func TestRTMPSubscribersReceiveUniqueIDs(t *testing.T) {
	hub, _ := newTestHub()
	stream, err := hub.GetOrCreate("live/unique")
	if err != nil {
		t.Fatal(err)
	}
	firstClient, firstServer := net.Pipe()
	defer firstClient.Close()
	defer firstServer.Close()
	secondClient, secondServer := net.Pipe()
	defer secondClient.Close()
	defer secondServer.Close()
	first := NewSubscriber("live/unique", firstServer, NewChunkWriter(firstServer, DefaultChunkSize), stream, nil)
	second := NewSubscriber("live/unique", secondServer, NewChunkWriter(secondServer, DefaultChunkSize), stream, nil)
	if first.ID() == second.ID() {
		t.Fatalf("RTMP subscriber IDs were reused: %q", first.ID())
	}
}
