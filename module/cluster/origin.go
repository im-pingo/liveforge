package cluster

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
)

// OriginPull manages pulling a single stream from an origin server.
type OriginPull struct {
	streamKey   string
	servers     []string
	stream      *core.Stream
	registry    *TransportRegistry
	health      *HealthTracker
	pool        *RelayPool
	retryMax    int
	retryDelay  time.Duration
	idleTimeout time.Duration
	metrics     *RelayMetrics

	mu        sync.Mutex
	closed    chan struct{}
	running   bool
	startedAt time.Time
	endpoint  string
}

// NewOriginPull creates a new origin pull instance.
func NewOriginPull(streamKey string, servers []string, stream *core.Stream, registry *TransportRegistry, health *HealthTracker, pool *RelayPool, retryMax int, retryDelay, idleTimeout time.Duration, relayMetrics ...*RelayMetrics) *OriginPull {
	op := &OriginPull{
		streamKey:   streamKey,
		servers:     servers,
		stream:      stream,
		registry:    registry,
		health:      health,
		pool:        pool,
		retryMax:    retryMax,
		retryDelay:  retryDelay,
		idleTimeout: idleTimeout,
		closed:      make(chan struct{}),
	}
	if len(relayMetrics) > 0 {
		op.metrics = relayMetrics[0]
	}
	return op
}

// Run tries each origin server in order, pulling media data and publishing
// it into the local stream. Retries on failure.
func (op *OriginPull) Run() {
	defer slog.Info("origin pull stopped", "module", "cluster", "stream", op.streamKey)

	for attempt := 0; ; attempt++ {
		select {
		case <-op.closed:
			return
		default:
		}

		if op.retryMax > 0 && attempt >= op.retryMax {
			slog.Warn("origin pull max retries exceeded", "module", "cluster",
				"stream", op.streamKey, "attempts", attempt)
			return
		}

		if attempt > 0 {
			delay := op.retryDelay * time.Duration(1<<min(attempt-1, 4)) // cap at 16x base
			if delay > 30*time.Second {
				delay = 30 * time.Second
			}
			select {
			case <-op.closed:
				return
			case <-time.After(delay):
			}
		}

		// Try each server in order
		pulled := false
		for _, server := range op.servers {
			select {
			case <-op.closed:
				return
			default:
			}

			url := fmt.Sprintf("%s/%s", server, op.streamKey)
			err := op.pullOnce(url)
			if err != nil {
				slog.Warn("origin pull failed", "module", "cluster",
					"stream", op.streamKey, "server", server, "error", err)
				continue
			}
			pulled = true
			break
		}

		if pulled {
			// Successful pull ended (stream ended on origin or idle timeout)
			return
		}
	}
}

// pullOnce connects to a single origin server and pulls the stream.
func (op *OriginPull) pullOnce(sourceURL string) error {
	transport, err := op.registry.Resolve(sourceURL)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		select {
		case <-op.closed:
			cancel()
		case <-ctx.Done():
		}
	}()

	if op.pool != nil {
		host := extractHost(sourceURL)
		if err := op.pool.Acquire(ctx, host); err != nil {
			return err
		}
		defer op.pool.Release(host)
	}

	op.mu.Lock()
	op.running = true
	op.startedAt = time.Now()
	op.endpoint = sourceURL
	op.mu.Unlock()
	defer func() {
		op.mu.Lock()
		op.running = false
		op.mu.Unlock()
	}()

	protocol := transport.Scheme()
	relayCtx := observeRelay(ctx, op.metrics, relayDirectionOrigin, protocol)
	op.metrics.RelayStarted(relayDirectionOrigin, protocol)
	err = transport.Pull(relayCtx, sourceURL, op.stream)
	flushRelayBytes(relayCtx)
	cancelled := ctx.Err() != nil
	metricErr := err
	if cancelled {
		metricErr = nil
	}
	op.metrics.RelayStopped(relayDirectionOrigin, protocol)
	op.metrics.RecordPull(protocol, 0, metricErr)
	if cancelled {
		return nil
	}
	if err != nil {
		if op.health != nil {
			op.health.RecordFailure(sourceURL)
		}
		return err
	}
	if op.health != nil {
		op.health.RecordSuccess(sourceURL)
	}
	return nil
}

func (op *OriginPull) statusSnapshot() (RelayStatus, bool) {
	op.mu.Lock()
	defer op.mu.Unlock()
	if !op.running {
		return RelayStatus{}, false
	}
	return RelayStatus{
		Direction: "origin",
		Protocol:  extractScheme(op.endpoint),
		StreamKey: op.streamKey,
		Endpoint:  statusEndpoint(op.endpoint),
		StartedAt: op.startedAt,
	}, true
}

// Close stops the origin pull.
func (op *OriginPull) Close() {
	op.mu.Lock()
	defer op.mu.Unlock()
	select {
	case <-op.closed:
	default:
		close(op.closed)
	}
}

// originPublisher implements core.Publisher for origin-pulled streams.
type originPublisher struct {
	id   string
	info *avframe.MediaInfo
}

var originPublisherSequence atomic.Uint64

func newOriginPublisher(kind, streamKey string, info *avframe.MediaInfo) *originPublisher {
	return &originPublisher{
		id:   fmt.Sprintf("%s-%s-%d", kind, streamKey, originPublisherSequence.Add(1)),
		info: info,
	}
}

func (p *originPublisher) ID() string                    { return p.id }
func (p *originPublisher) MediaInfo() *avframe.MediaInfo { return p.info }
func (p *originPublisher) Close() error                  { return nil }

// OriginManager manages origin pulls for streams that have subscribers but no publisher.
type OriginManager struct {
	hub         *core.StreamHub
	eventBus    *core.EventBus
	scheduler   *Scheduler
	registry    *TransportRegistry
	health      *HealthTracker
	pool        *RelayPool
	retryMax    int
	retryDelay  time.Duration
	idleTimeout time.Duration

	mu      sync.Mutex
	active  map[string]*OriginPull
	closed  chan struct{}
	close   sync.Once
	metrics *RelayMetrics
}

// NewOriginManager creates a new origin manager.
func NewOriginManager(hub *core.StreamHub, bus *core.EventBus, scheduler *Scheduler, registry *TransportRegistry, health *HealthTracker, pool *RelayPool, retryMax int, retryDelay, idleTimeout time.Duration, relayMetrics ...*RelayMetrics) *OriginManager {
	if retryMax <= 0 {
		retryMax = 3
	}
	if retryDelay <= 0 {
		retryDelay = 2 * time.Second
	}
	if idleTimeout <= 0 {
		idleTimeout = 30 * time.Second
	}
	om := &OriginManager{
		hub:         hub,
		eventBus:    bus,
		scheduler:   scheduler,
		registry:    registry,
		health:      health,
		pool:        pool,
		retryMax:    retryMax,
		retryDelay:  retryDelay,
		idleTimeout: idleTimeout,
		active:      make(map[string]*OriginPull),
		closed:      make(chan struct{}),
	}
	if len(relayMetrics) > 0 {
		om.metrics = relayMetrics[0]
	}
	return om
}

// Hooks returns event hooks for the origin manager.
func (om *OriginManager) Hooks() []core.HookRegistration {
	return []core.HookRegistration{
		{
			Event:    core.EventSubscribe,
			Mode:     core.HookAsync,
			Priority: 100,
			Consumer: "cluster-origin",
			Handler:  om.onSubscribe,
		},
	}
}

func (om *OriginManager) UpdatePolicy(cfg config.OriginConfig, health *HealthTracker) {
	retryMax := cfg.RetryMax
	if retryMax <= 0 {
		retryMax = 3
	}
	retryDelay := cfg.RetryDelay
	if retryDelay <= 0 {
		retryDelay = 2 * time.Second
	}
	idleTimeout := cfg.IdleTimeout
	if idleTimeout <= 0 {
		idleTimeout = 30 * time.Second
	}
	om.mu.Lock()
	om.scheduler = NewScheduler(cfg.ScheduleURL, cfg.Servers, cfg.SchedulePriority, cfg.ScheduleTimeout)
	om.retryMax = retryMax
	om.retryDelay = retryDelay
	om.idleTimeout = idleTimeout
	om.health = health
	om.mu.Unlock()
}

func (om *OriginManager) onSubscribe(ctx *core.EventContext) error {
	stream, ok := om.hub.Find(ctx.StreamKey)
	if !ok {
		return nil
	}

	// Only pull if there's no publisher yet
	if stream.Publisher() != nil {
		return nil
	}

	om.mu.Lock()
	defer om.mu.Unlock()

	// Don't create duplicate pulls
	if _, exists := om.active[ctx.StreamKey]; exists {
		return nil
	}

	servers, err := om.scheduler.Resolve("origin", ctx.StreamKey)
	if err != nil {
		slog.Warn("origin schedule resolve failed", "module", "cluster",
			"stream", ctx.StreamKey, "error", err)
		return nil
	}

	if om.health != nil {
		servers = om.health.FilterHealthy(servers)
	}

	op := NewOriginPull(ctx.StreamKey, servers, stream, om.registry, om.health, om.pool, om.retryMax, om.retryDelay, om.idleTimeout, om.metrics)
	om.active[ctx.StreamKey] = op

	om.eventBus.Emit(core.EventOriginPullStart, &core.EventContext{ //nolint:errcheck
		StreamKey: ctx.StreamKey,
	})

	go func() {
		op.Run()

		om.mu.Lock()
		delete(om.active, ctx.StreamKey)
		om.mu.Unlock()

		om.eventBus.Emit(core.EventOriginPullStop, &core.EventContext{ //nolint:errcheck
			StreamKey: ctx.StreamKey,
		})
	}()

	return nil
}

// Close stops all active origin pulls.
func (om *OriginManager) Close() {
	om.close.Do(func() {
		close(om.closed)

		om.mu.Lock()
		defer om.mu.Unlock()

		for key, op := range om.active {
			op.Close()
			delete(om.active, key)
		}
	})
}

// ActiveCount returns the number of active origin pulls.
func (om *OriginManager) ActiveCount() int {
	om.mu.Lock()
	defer om.mu.Unlock()
	return len(om.active)
}

// StatusSnapshot returns active origin transport state.
func (om *OriginManager) StatusSnapshot() []RelayStatus {
	om.mu.Lock()
	pulls := make([]*OriginPull, 0, len(om.active))
	for _, pull := range om.active {
		pulls = append(pulls, pull)
	}
	om.mu.Unlock()

	status := make([]RelayStatus, 0, len(pulls))
	for _, pull := range pulls {
		if snapshot, ok := pull.statusSnapshot(); ok {
			status = append(status, snapshot)
		}
	}
	return status
}
