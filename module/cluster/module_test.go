package cluster

import (
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
)

func newTestHub() (*core.StreamHub, *core.EventBus) {
	bus := core.NewEventBus()
	hub := core.NewStreamHub(config.StreamConfig{RingBufferSize: 256}, config.LimitsConfig{}, bus)
	return hub, bus
}

func TestModuleName(t *testing.T) {
	m := NewModule()
	if m.Name() != "cluster" {
		t.Errorf("Name = %q, want %q", m.Name(), "cluster")
	}
}

func TestModuleHooksNilWhenDisabled(t *testing.T) {
	m := NewModule()
	hooks := m.Hooks()
	if len(hooks) != 0 {
		t.Errorf("Hooks should be empty when disabled, got %d", len(hooks))
	}
}

func TestModuleInitDisabled(t *testing.T) {
	cfg := &config.Config{}
	cfg.Stream.RingBufferSize = 256
	// Both forward and origin disabled by default

	srv := core.NewServer(cfg)
	m := NewModule()
	srv.RegisterModule(m)

	if err := m.Init(srv); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer m.Close()

	if m.ForwardManager() != nil {
		t.Error("ForwardManager should be nil when disabled")
	}
	if m.OriginManager() != nil {
		t.Error("OriginManager should be nil when disabled")
	}
	if len(m.Hooks()) != 0 {
		t.Errorf("Hooks should be empty, got %d", len(m.Hooks()))
	}
}

func TestModuleInitForwardEnabled(t *testing.T) {
	cfg := &config.Config{}
	cfg.Stream.RingBufferSize = 256
	cfg.Cluster.Forward.Enabled = true
	cfg.Cluster.Forward.Targets = []string{"rtmp://target1/live/stream", "rtmp://target2/live/stream"}
	cfg.Cluster.Forward.RetryMax = 5
	cfg.Cluster.Forward.RetryInterval = 3 * time.Second

	srv := core.NewServer(cfg)
	m := NewModule()

	if err := m.Init(srv); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer m.Close()

	if m.ForwardManager() == nil {
		t.Fatal("ForwardManager should not be nil when enabled")
	}

	hooks := m.Hooks()
	if len(hooks) != 2 {
		t.Errorf("expected 2 hooks (publish + publishStop), got %d", len(hooks))
	}
}

func TestModuleInitOriginEnabled(t *testing.T) {
	cfg := &config.Config{}
	cfg.Stream.RingBufferSize = 256
	cfg.Cluster.Origin.Enabled = true
	cfg.Cluster.Origin.Servers = []string{"rtmp://origin1/live", "rtmp://origin2/live"}
	cfg.Cluster.Origin.RetryMax = 3
	cfg.Cluster.Origin.IdleTimeout = 30 * time.Second

	srv := core.NewServer(cfg)
	m := NewModule()

	if err := m.Init(srv); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer m.Close()

	if m.OriginManager() == nil {
		t.Fatal("OriginManager should not be nil when enabled")
	}

	hooks := m.Hooks()
	if len(hooks) != 1 {
		t.Errorf("expected 1 hook (subscribe), got %d", len(hooks))
	}
}

func TestModuleInitBothEnabled(t *testing.T) {
	cfg := &config.Config{}
	cfg.Stream.RingBufferSize = 256
	cfg.Cluster.Forward.Enabled = true
	cfg.Cluster.Forward.Targets = []string{"rtmp://target/live/stream"}
	cfg.Cluster.Origin.Enabled = true
	cfg.Cluster.Origin.Servers = []string{"rtmp://origin/live"}

	srv := core.NewServer(cfg)
	m := NewModule()

	if err := m.Init(srv); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer m.Close()

	hooks := m.Hooks()
	if len(hooks) != 3 {
		t.Errorf("expected 3 hooks, got %d", len(hooks))
	}
}

func TestModuleCloseIdempotent(t *testing.T) {
	m := NewModule()
	// Close on uninitialized module should not panic
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("double Close: %v", err)
	}
}

func newTestRegistry() *TransportRegistry {
	registry := NewTransportRegistry()
	registry.Register(NewRTMPTransport())
	return registry
}

func TestForwardManagerDefaults(t *testing.T) {
	hub, bus := newTestHub()
	fm := NewForwardManager(hub, bus, NewScheduler("", []string{"rtmp://target/live/stream"}, "", 0), newTestRegistry(), 0, 0)

	if fm.retryMax != 3 {
		t.Errorf("retryMax = %d, want 3", fm.retryMax)
	}
	if fm.retryDel != 5*time.Second {
		t.Errorf("retryDel = %v, want 5s", fm.retryDel)
	}
	if fm.ActiveCount() != 0 {
		t.Errorf("ActiveCount = %d, want 0", fm.ActiveCount())
	}
}

func TestForwardManagerOnPublishNoStream(t *testing.T) {
	hub, bus := newTestHub()
	fm := NewForwardManager(hub, bus, NewScheduler("", []string{"rtmp://target/live/stream"}, "", 0), newTestRegistry(), 1, time.Millisecond)
	defer fm.Close()

	// Publish event for non-existent stream should not create targets
	err := fm.onPublish(&core.EventContext{StreamKey: "nonexistent/stream"})
	if err != nil {
		t.Fatalf("onPublish: %v", err)
	}
	if fm.ActiveCount() != 0 {
		t.Errorf("ActiveCount = %d, want 0", fm.ActiveCount())
	}
}

func TestForwardManagerOnPublishStop(t *testing.T) {
	hub, bus := newTestHub()
	fm := NewForwardManager(hub, bus, NewScheduler("", []string{"rtmp://127.0.0.1:19999/live/stream"}, "", 0), newTestRegistry(), 1, time.Millisecond)

	stream, _ := hub.GetOrCreate("live/test")
	// Set a dummy publisher so the stream is in publishing state
	pub := &originPublisher{id: "test", info: &avframe.MediaInfo{}}
	stream.SetPublisher(pub)

	// Trigger publish event — targets will be created but fail to connect (no server at :19999)
	fm.onPublish(&core.EventContext{StreamKey: "live/test"})

	if fm.ActiveCount() != 1 {
		t.Errorf("ActiveCount = %d, want 1", fm.ActiveCount())
	}

	// PublishStop should clean up
	fm.onPublishStop(&core.EventContext{StreamKey: "live/test"})

	// Give goroutines a moment to clean up
	time.Sleep(50 * time.Millisecond)

	if fm.ActiveCount() != 0 {
		t.Errorf("ActiveCount after stop = %d, want 0", fm.ActiveCount())
	}
}

func TestForwardManagerDuplicatePublish(t *testing.T) {
	hub, bus := newTestHub()
	fm := NewForwardManager(hub, bus, NewScheduler("", []string{"rtmp://127.0.0.1:19999/live/stream"}, "", 0), newTestRegistry(), 1, time.Millisecond)
	defer fm.Close()

	stream, _ := hub.GetOrCreate("live/test")
	pub := &originPublisher{id: "test", info: &avframe.MediaInfo{}}
	stream.SetPublisher(pub)

	fm.onPublish(&core.EventContext{StreamKey: "live/test"})
	fm.onPublish(&core.EventContext{StreamKey: "live/test"}) // duplicate

	if fm.ActiveCount() != 1 {
		t.Errorf("ActiveCount = %d, want 1 (no duplicate)", fm.ActiveCount())
	}
}

func TestForwardManagerClose(t *testing.T) {
	hub, bus := newTestHub()
	fm := NewForwardManager(hub, bus, NewScheduler("", []string{"rtmp://127.0.0.1:19999/live/stream"}, "", 0), newTestRegistry(), 1, time.Millisecond)

	stream, _ := hub.GetOrCreate("live/test")
	pub := &originPublisher{id: "test", info: &avframe.MediaInfo{}}
	stream.SetPublisher(pub)

	fm.onPublish(&core.EventContext{StreamKey: "live/test"})
	fm.Close()

	time.Sleep(50 * time.Millisecond)
	if fm.ActiveCount() != 0 {
		t.Errorf("ActiveCount after close = %d, want 0", fm.ActiveCount())
	}
}

func TestOriginManagerDefaults(t *testing.T) {
	hub, bus := newTestHub()
	om := NewOriginManager(hub, bus, NewScheduler("", []string{"rtmp://origin/live"}, "", 0), 0, 0, 0)

	if om.retryMax != 3 {
		t.Errorf("retryMax = %d, want 3", om.retryMax)
	}
	if om.idleTimeout != 30*time.Second {
		t.Errorf("idleTimeout = %v, want 30s", om.idleTimeout)
	}
	if om.ActiveCount() != 0 {
		t.Errorf("ActiveCount = %d, want 0", om.ActiveCount())
	}
}

func TestOriginManagerOnSubscribeNoStream(t *testing.T) {
	hub, bus := newTestHub()
	om := NewOriginManager(hub, bus, NewScheduler("", []string{"rtmp://127.0.0.1:19999/live"}, "", 0), 1, time.Second, time.Second)
	defer om.Close()

	err := om.onSubscribe(&core.EventContext{StreamKey: "nonexistent/stream"})
	if err != nil {
		t.Fatalf("onSubscribe: %v", err)
	}
	if om.ActiveCount() != 0 {
		t.Errorf("ActiveCount = %d, want 0", om.ActiveCount())
	}
}

func TestOriginManagerOnSubscribeWithPublisher(t *testing.T) {
	hub, bus := newTestHub()
	om := NewOriginManager(hub, bus, NewScheduler("", []string{"rtmp://127.0.0.1:19999/live"}, "", 0), 1, time.Second, time.Second)
	defer om.Close()

	stream, _ := hub.GetOrCreate("live/test")
	pub := &originPublisher{id: "test", info: &avframe.MediaInfo{}}
	stream.SetPublisher(pub)

	// Subscribe when publisher exists should not trigger origin pull
	om.onSubscribe(&core.EventContext{StreamKey: "live/test"})
	if om.ActiveCount() != 0 {
		t.Errorf("ActiveCount = %d, want 0 (publisher exists)", om.ActiveCount())
	}
}

func TestOriginManagerOnSubscribeTriggersPull(t *testing.T) {
	hub, bus := newTestHub()
	om := NewOriginManager(hub, bus, NewScheduler("", []string{"rtmp://127.0.0.1:19999/live"}, "", 0), 1, time.Second, time.Second)

	// Create stream without publisher
	hub.GetOrCreate("live/test")

	om.onSubscribe(&core.EventContext{StreamKey: "live/test"})

	if om.ActiveCount() != 1 {
		t.Errorf("ActiveCount = %d, want 1", om.ActiveCount())
	}

	// Wait for pull to fail and clean up (connects to non-existent server)
	time.Sleep(500 * time.Millisecond)

	om.Close()
}

func TestOriginManagerDuplicateSubscribe(t *testing.T) {
	hub, bus := newTestHub()
	om := NewOriginManager(hub, bus, NewScheduler("", []string{"rtmp://127.0.0.1:19999/live"}, "", 0), 1, time.Second, time.Second)
	defer om.Close()

	hub.GetOrCreate("live/test")

	om.onSubscribe(&core.EventContext{StreamKey: "live/test"})
	om.onSubscribe(&core.EventContext{StreamKey: "live/test"}) // duplicate

	if om.ActiveCount() != 1 {
		t.Errorf("ActiveCount = %d, want 1 (no duplicate)", om.ActiveCount())
	}
}

func TestOriginManagerClose(t *testing.T) {
	hub, bus := newTestHub()
	om := NewOriginManager(hub, bus, NewScheduler("", []string{"rtmp://127.0.0.1:19999/live"}, "", 0), 1, time.Second, time.Second)

	hub.GetOrCreate("live/test")
	om.onSubscribe(&core.EventContext{StreamKey: "live/test"})

	om.Close()
	time.Sleep(50 * time.Millisecond)

	if om.ActiveCount() != 0 {
		t.Errorf("ActiveCount after close = %d, want 0", om.ActiveCount())
	}
}

func TestOriginPublisher(t *testing.T) {
	pub := &originPublisher{
		id:   "test-origin",
		info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264},
	}

	if pub.ID() != "test-origin" {
		t.Errorf("ID = %q", pub.ID())
	}
	if pub.MediaInfo().VideoCodec != avframe.CodecH264 {
		t.Errorf("VideoCodec = %v", pub.MediaInfo().VideoCodec)
	}
	if err := pub.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestForwardTargetClose(t *testing.T) {
	hub, bus := newTestHub()
	stream, _ := hub.GetOrCreate("live/test")
	_ = bus

	ft := NewForwardTarget("live/test", "rtmp://127.0.0.1:19999/live/test", stream, NewRTMPTransport(), 1, time.Millisecond)

	// Close before Run
	ft.Close()

	// Double close should not panic
	ft.Close()
}

func TestOriginPullClose(t *testing.T) {
	hub, _ := newTestHub()
	stream, _ := hub.GetOrCreate("live/test")

	op := NewOriginPull("live/test", []string{"rtmp://127.0.0.1:19999/live"}, stream, 1, time.Second, time.Second)

	// Close before Run
	op.Close()

	// Double close should not panic
	op.Close()
}

func TestForwardTargetRunWithClosedTarget(t *testing.T) {
	hub, _ := newTestHub()
	stream, _ := hub.GetOrCreate("live/test")

	ft := NewForwardTarget("live/test", "rtmp://127.0.0.1:19999/live/test", stream, NewRTMPTransport(), 1, time.Millisecond)
	ft.Close()

	// Run should return immediately when already closed
	done := make(chan struct{})
	go func() {
		ft.Run()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Error("Run did not return after Close")
	}
}

func TestOriginPullRunWithClosedPull(t *testing.T) {
	hub, _ := newTestHub()
	stream, _ := hub.GetOrCreate("live/test")

	op := NewOriginPull("live/test", []string{"rtmp://127.0.0.1:19999/live"}, stream, 1, time.Second, time.Second)
	op.Close()

	done := make(chan struct{})
	go func() {
		op.Run()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Error("Run did not return after Close")
	}
}
