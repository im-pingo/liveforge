package rtmp

import (
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
)

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
