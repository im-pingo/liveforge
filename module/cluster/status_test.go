package cluster

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

type blockingRelayTransport struct {
	scheme  string
	started chan struct{}
	release chan struct{}
	err     error
	bytes   int64
	once    sync.Once
}

func (t *blockingRelayTransport) Scheme() string { return t.scheme }

func (t *blockingRelayTransport) Push(ctx context.Context, _ string, _ *core.Stream) error {
	t.once.Do(func() { close(t.started) })
	recordRelayBytes(ctx, t.bytes)
	markRelayConnected(ctx)
	select {
	case <-t.release:
		return t.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (t *blockingRelayTransport) Pull(ctx context.Context, _ string, _ *core.Stream) error {
	return t.Push(ctx, "", nil)
}

func (t *blockingRelayTransport) Close() error { return nil }

func TestRelayMetricsPacketLossUsesBoundedLabels(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewRelayMetricsWithRegistry(reg)

	m.RecordPacketLoss("live/one", "forward", 0.02)
	m.RecordPacketLoss("live/two", "forward", 0.04)

	// Stream keys must not become metric labels: a deployment can create an
	// unbounded number of streams over its lifetime.
	expected := `
		# HELP cluster_rtp_packet_loss_ratio RTP packet loss ratio.
		# TYPE cluster_rtp_packet_loss_ratio gauge
		cluster_rtp_packet_loss_ratio{direction="forward"} 0.04
	`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected), "cluster_rtp_packet_loss_ratio"); err != nil {
		t.Fatalf("packet loss labels are not bounded: %v", err)
	}
}

func TestSequenceLossTrackerReportsMissingPacketsAcrossWrap(t *testing.T) {
	tracker := newSequenceLossTracker()
	for _, seq := range []uint16{65534, 65535, 1} {
		tracker.Observe(seq)
	}

	if got, want := tracker.Ratio(), 0.25; got != want {
		t.Fatalf("loss ratio = %v, want %v", got, want)
	}
}

func TestSequenceLossTrackerDoesNotLetDuplicatesHideLoss(t *testing.T) {
	tracker := newSequenceLossTracker()
	for _, seq := range []uint16{10, 12, 11, 11, 14} {
		tracker.Observe(seq)
	}

	if got, want := tracker.Ratio(), 0.2; got != want {
		t.Fatalf("loss ratio = %v, want %v", got, want)
	}
}

func TestForwardTargetRecordsLifecycleMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics := NewRelayMetricsWithRegistry(reg)
	transport := &blockingRelayTransport{
		scheme:  "test",
		started: make(chan struct{}),
		release: make(chan struct{}),
		bytes:   4,
	}
	hub, _ := newTestHub()
	stream, _ := hub.GetOrCreate("live/test")
	target := NewForwardTarget("live/test", "test://peer/live/test", stream, transport, nil, nil, 1, time.Millisecond, metrics)

	done := make(chan struct{})
	go func() {
		target.Run()
		close(done)
	}()
	select {
	case <-transport.started:
	case <-time.After(time.Second):
		t.Fatal("forward transport did not start")
	}

	active := `
		# HELP cluster_relay_active Number of active relay connections.
		# TYPE cluster_relay_active gauge
		cluster_relay_active{direction="forward",protocol="test"} 1
	`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(active), "cluster_relay_active"); err != nil {
		t.Fatalf("active relay metric: %v", err)
	}

	target.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("forward target did not stop after Close")
	}

	inactive := `
		# HELP cluster_relay_active Number of active relay connections.
		# TYPE cluster_relay_active gauge
		cluster_relay_active{direction="forward",protocol="test"} 0
	`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(inactive), "cluster_relay_active"); err != nil {
		t.Fatalf("inactive relay metric: %v", err)
	}
	forwardBytes := `
		# HELP cluster_relay_bytes_total Total bytes relayed.
		# TYPE cluster_relay_bytes_total counter
		cluster_relay_bytes_total{direction="forward",protocol="test"} 4
	`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(forwardBytes), "cluster_relay_bytes_total"); err != nil {
		t.Fatalf("forward byte metric: %v", err)
	}

	close(transport.release)
	target.Close()
}

type writingRelayTransport struct{}

func (t *writingRelayTransport) Scheme() string { return "write" }
func (t *writingRelayTransport) Push(context.Context, string, *core.Stream) error {
	return nil
}
func (t *writingRelayTransport) Pull(ctx context.Context, _ string, _ *core.Stream) error {
	markRelayConnected(ctx)
	recordRelayBytes(ctx, 4)
	return nil
}
func (t *writingRelayTransport) Close() error { return nil }

func TestOriginPullRecordsBytesFromActualStreamWrite(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics := NewRelayMetricsWithRegistry(reg)
	hub, _ := newTestHub()
	stream, _ := hub.GetOrCreate("live/test")
	registry := newTestRegistry()
	registry.Register(&writingRelayTransport{})
	pull := NewOriginPull("live/test", nil, stream, registry, nil, nil, 1, time.Millisecond, time.Second, metrics)

	if err := pull.pullOnce("write://peer/live/test"); err != nil {
		t.Fatalf("pullOnce: %v", err)
	}

	expected := `
		# HELP cluster_relay_bytes_total Total bytes relayed.
		# TYPE cluster_relay_bytes_total counter
		cluster_relay_bytes_total{direction="origin",protocol="write"} 4
	`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected), "cluster_relay_bytes_total"); err != nil {
		t.Fatalf("origin byte metric: %v", err)
	}
}

func TestOriginPullRecordsErrorAndLatencyMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics := NewRelayMetricsWithRegistry(reg)
	transport := &blockingRelayTransport{
		scheme:  "test",
		started: make(chan struct{}),
		release: make(chan struct{}),
		err:     errors.New("upstream failed"),
	}
	hub, _ := newTestHub()
	stream, _ := hub.GetOrCreate("live/test")
	pull := NewOriginPull("live/test", nil, stream, newTestRegistry(), nil, nil, 1, time.Millisecond, time.Second, metrics)
	pull.registry.Register(transport)

	done := make(chan error, 1)
	go func() { done <- pull.pullOnce("test://peer/live/test") }()
	select {
	case <-transport.started:
	case <-time.After(time.Second):
		t.Fatal("origin transport did not start")
	}
	close(transport.release)
	if err := <-done; err == nil {
		t.Fatal("pullOnce returned nil for failed transport")
	}

	pull.Close()
	active := `
		# HELP cluster_relay_active Number of active relay connections.
		# TYPE cluster_relay_active gauge
		cluster_relay_active{direction="origin",protocol="test"} 0
	`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(active), "cluster_relay_active"); err != nil {
		t.Fatalf("origin active relay metric: %v", err)
	}

	errExpected := `
		# HELP cluster_relay_errors_total Total relay errors.
		# TYPE cluster_relay_errors_total counter
		cluster_relay_errors_total{direction="origin",error_type="connection",protocol="test"} 1
	`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(errExpected), "cluster_relay_errors_total"); err != nil {
		t.Fatalf("origin error metric: %v", err)
	}
	if got := testutil.CollectAndCount(metrics.latency); got != 1 {
		t.Fatalf("origin latency series = %d, want 1", got)
	}
}

func TestModuleClusterStatusIsBoundedAndIncludesHealth(t *testing.T) {
	hub, bus := newTestHub()
	health := NewHealthTracker(config.HealthCheckConfig{
		Enabled:        true,
		EvictThreshold: 1,
		Interval:       time.Hour,
	})
	defer health.Close()
	health.RecordFailure("test://unhealthy:1234/live")

	fm := NewForwardManager(hub, bus, NewScheduler("", nil, "", 0), newTestRegistry(), health, nil, 1, time.Millisecond)
	om := NewOriginManager(hub, bus, NewScheduler("", nil, "", 0), newTestRegistry(), nil, nil, 1, time.Millisecond, time.Second)
	defer fm.Close()
	defer om.Close()

	stream, _ := hub.GetOrCreate("live/test")
	transport := &blockingRelayTransport{
		scheme:  "test",
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	target := NewForwardTarget("live/test", "test://user:secret@peer/live/test?token=secret#fragment", stream, transport, health, nil, 1, time.Millisecond)
	fm.mu.Lock()
	fm.active["live/test"] = []*ForwardTarget{target}
	fm.mu.Unlock()
	originTransport := &blockingRelayTransport{
		scheme:  "test",
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	originRegistry := newTestRegistry()
	originRegistry.Register(originTransport)
	originPull := NewOriginPull("live/origin", nil, stream, originRegistry, nil, nil, 1, time.Millisecond, time.Second)
	om.mu.Lock()
	om.active["live/origin"] = originPull
	om.mu.Unlock()
	done := make(chan struct{})
	go func() {
		target.Run()
		close(done)
	}()
	originDone := make(chan error, 1)
	go func() { originDone <- originPull.pullOnce("test://origin/live/origin") }()
	select {
	case <-transport.started:
	case <-time.After(time.Second):
		t.Fatal("forward transport did not start")
	}
	select {
	case <-originTransport.started:
	case <-time.After(time.Second):
		t.Fatal("origin transport did not start")
	}

	status := (&Module{forward: fm, origin: om, health: health}).ClusterStatus()
	if status.ActiveForwards != 1 || status.ActiveOrigins != 1 {
		t.Fatalf("active status = %+v", status)
	}
	if len(status.Relays) != 2 || status.Relays[0].StreamKey != "live/test" || status.Relays[0].Endpoint != "test://peer/live/test" {
		t.Fatalf("relay snapshot = %+v", status.Relays)
	}
	if status.Relays[1].Direction != "origin" || status.Relays[1].StreamKey != "live/origin" {
		t.Fatalf("origin relay snapshot = %+v", status.Relays[1])
	}
	if len(status.Peers) != 1 || status.Peers[0].Host != "unhealthy:1234" || !status.Peers[0].Evicted {
		t.Fatalf("peer snapshot = %+v", status.Peers)
	}
	if len(status.Relays) > maxClusterStatusRelays {
		t.Fatalf("relay status is unbounded: %d", len(status.Relays))
	}

	target.Close()
	originPull.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("forward target did not stop")
	}
	select {
	case <-originDone:
	case <-time.After(time.Second):
		t.Fatal("origin pull did not stop")
	}
}

func TestModuleClusterStatusCapsRelayAndPeerSnapshots(t *testing.T) {
	hub, bus := newTestHub()
	health := NewHealthTracker(config.HealthCheckConfig{
		Enabled:        true,
		EvictThreshold: 1,
		Interval:       time.Hour,
	})
	defer health.Close()
	fm := NewForwardManager(hub, bus, NewScheduler("", nil, "", 0), newTestRegistry(), health, nil, 1, time.Millisecond)
	defer fm.Close()

	for i := 0; i < maxClusterStatusRelays+10; i++ {
		streamKey := fmt.Sprintf("live/%03d", i)
		stream, _ := hub.GetOrCreate(streamKey)
		transport := &blockingRelayTransport{scheme: "test"}
		target := NewForwardTarget(streamKey, fmt.Sprintf("test://peer/%03d", i), stream, transport, health, nil, 1, time.Millisecond)
		target.mu.Lock()
		target.running = true
		target.startedAt = time.Now()
		target.mu.Unlock()
		fm.active[streamKey] = []*ForwardTarget{target}
		health.RecordFailure(fmt.Sprintf("test://peer-%03d:1234/live", i))
	}

	status := (&Module{forward: fm, health: health}).ClusterStatus()
	if got := status.ActiveForwards; got != maxClusterStatusRelays+10 {
		t.Fatalf("active forwards = %d, want %d", got, maxClusterStatusRelays+10)
	}
	if got := len(status.Relays); got != maxClusterStatusRelays {
		t.Fatalf("relay snapshot length = %d, want %d", got, maxClusterStatusRelays)
	}
	if got := len(status.Peers); got != maxClusterStatusPeers {
		t.Fatalf("peer snapshot length = %d, want %d", got, maxClusterStatusPeers)
	}
	if !status.Truncated {
		t.Fatal("bounded status did not report truncation")
	}
}
