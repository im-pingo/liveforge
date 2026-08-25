package srt

import (
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gosrt "github.com/datarhei/gosrt"
	"github.com/datarhei/gosrt/packet"
	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
)

type subscriberLifecycleConn struct {
	streamID     string
	socketID     uint32
	peerSocketID uint32
}

func (c *subscriberLifecycleConn) Read([]byte) (int, error)           { return 0, io.EOF }
func (c *subscriberLifecycleConn) ReadPacket() (packet.Packet, error) { return nil, io.EOF }
func (c *subscriberLifecycleConn) Write(p []byte) (int, error)        { return len(p), nil }
func (c *subscriberLifecycleConn) WritePacket(packet.Packet) error    { return nil }
func (c *subscriberLifecycleConn) Close() error                       { return nil }
func (c *subscriberLifecycleConn) LocalAddr() net.Addr                { return &net.UDPAddr{} }
func (c *subscriberLifecycleConn) RemoteAddr() net.Addr {
	return &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 6000}
}
func (c *subscriberLifecycleConn) SetDeadline(time.Time) error      { return nil }
func (c *subscriberLifecycleConn) SetReadDeadline(time.Time) error  { return nil }
func (c *subscriberLifecycleConn) SetWriteDeadline(time.Time) error { return nil }
func (c *subscriberLifecycleConn) SocketId() uint32                 { return c.socketID }
func (c *subscriberLifecycleConn) PeerSocketId() uint32             { return c.peerSocketID }
func (c *subscriberLifecycleConn) StreamId() string                 { return c.streamID }
func (c *subscriberLifecycleConn) Stats(*gosrt.Statistics)          {}
func (c *subscriberLifecycleConn) Version() uint32                  { return 5 }

type subscriberLifecycleRequest struct {
	*subscriberLifecycleConn
}

func (r *subscriberLifecycleRequest) IsEncrypted() bool                        { return false }
func (r *subscriberLifecycleRequest) SetPassphrase(string) error               { return nil }
func (r *subscriberLifecycleRequest) SetRejectionReason(gosrt.RejectionReason) {}
func (r *subscriberLifecycleRequest) Accept() (gosrt.Conn, error) {
	return r.subscriberLifecycleConn, nil
}
func (r *subscriberLifecycleRequest) Reject(gosrt.RejectionReason) {}

type subscriberLifecyclePublisher struct{}

func (subscriberLifecyclePublisher) ID() string { return "source" }
func (subscriberLifecyclePublisher) MediaInfo() *avframe.MediaInfo {
	return &avframe.MediaInfo{VideoCodec: avframe.CodecH264}
}
func (subscriberLifecyclePublisher) Close() error { return nil }

func TestSRTSubscribeAdmissionOnlyRunsSynchronousAuthorization(t *testing.T) {
	bus := core.NewEventBus()
	server := core.NewServer(&config.Config{Stream: config.StreamConfig{RingBufferSize: 256}})
	m := NewModule()
	m.server = server
	m.eventBus = bus
	var authorizations atomic.Int32
	lifecycleStart := make(chan struct{}, 1)
	bus.Register(core.HookRegistration{Event: core.EventSubscribe, Mode: core.HookSync, Handler: func(*core.EventContext) error {
		authorizations.Add(1)
		return nil
	}})
	bus.Register(core.HookRegistration{Event: core.EventSubscribe, Mode: core.HookAsync, Handler: func(*core.EventContext) error {
		lifecycleStart <- struct{}{}
		return nil
	}})
	req := &subscriberLifecycleRequest{subscriberLifecycleConn: &subscriberLifecycleConn{
		streamID:     "subscribe:/live/auth",
		socketID:     10,
		peerSocketID: 20,
	}}

	if got := m.handleConnect(req); got != gosrt.SUBSCRIBE {
		t.Fatalf("connection type = %v, want SUBSCRIBE", got)
	}
	defer server.ReleaseConn()
	if got := authorizations.Load(); got != 1 {
		t.Fatalf("authorization calls = %d, want 1", got)
	}
	select {
	case <-lifecycleStart:
		t.Fatal("SRT admission emitted a lifecycle start before a subscriber existed")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestSRTSubscriberLifecycleSerializesBlockedStartBeforeStop(t *testing.T) {
	bus := core.NewEventBus()
	hub := core.NewStreamHub(config.StreamConfig{RingBufferSize: 256}, config.LimitsConfig{}, bus)
	stream, err := hub.GetOrCreate("live/lifecycle")
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.SetPublisher(subscriberLifecyclePublisher{}); err != nil {
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

	conn := &subscriberLifecycleConn{streamID: "subscribe:/live/lifecycle", socketID: 100, peerSocketID: 200}
	sub := NewSubscriber(conn, "live/lifecycle", hub, bus, nil)
	runDone := make(chan struct{})
	go func() {
		sub.Run()
		close(runDone)
	}()

	var start *core.EventContext
	select {
	case start = <-starts:
	case <-time.After(2 * time.Second):
		t.Fatal("SRT subscribe start did not run")
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
		t.Fatal("SRT subscribe stop did not run")
	}
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("SRT subscriber did not exit")
	}
	if stopOvertook {
		t.Error("SRT subscribe stop overtook the blocked start")
	}
	if start.SubscriberID == "" {
		t.Error("SRT subscribe start has an empty subscriber ID")
	}
	if stop.SubscriberID != start.SubscriberID {
		t.Errorf("SRT subscribe stop ID = %q, want %q", stop.SubscriberID, start.SubscriberID)
	}
}

func TestSRTSubscribersReceiveUniqueIDsWhenSocketIdentityIsReused(t *testing.T) {
	bus := core.NewEventBus()
	hub := core.NewStreamHub(config.StreamConfig{RingBufferSize: 256}, config.LimitsConfig{}, bus)
	first := NewSubscriber(
		&subscriberLifecycleConn{streamID: "subscribe:/live/unique", socketID: 100, peerSocketID: 200},
		"live/unique",
		hub,
		bus,
		nil,
	)
	second := NewSubscriber(
		&subscriberLifecycleConn{streamID: "subscribe:/live/unique", socketID: 100, peerSocketID: 200},
		"live/unique",
		hub,
		bus,
		nil,
	)
	if first.id == second.id {
		t.Fatalf("SRT subscriber IDs were reused: %q", first.id)
	}
}
