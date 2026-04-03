package cluster

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
)

// OriginPull manages pulling a single stream from an origin server.
type OriginPull struct {
	streamKey   string
	servers     []string
	stream      *core.Stream
	registry    *TransportRegistry
	retryMax    int
	retryDelay  time.Duration
	idleTimeout time.Duration

	mu     sync.Mutex
	closed chan struct{}
}

// NewOriginPull creates a new origin pull instance.
func NewOriginPull(streamKey string, servers []string, stream *core.Stream, registry *TransportRegistry, retryMax int, retryDelay, idleTimeout time.Duration) *OriginPull {
	return &OriginPull{
		streamKey:   streamKey,
		servers:     servers,
		stream:      stream,
		registry:    registry,
		retryMax:    retryMax,
		retryDelay:  retryDelay,
		idleTimeout: idleTimeout,
		closed:      make(chan struct{}),
	}
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

	return transport.Pull(ctx, sourceURL, op.stream)
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

func (p *originPublisher) ID() string                    { return p.id }
func (p *originPublisher) MediaInfo() *avframe.MediaInfo { return p.info }
func (p *originPublisher) Close() error                  { return nil }

// OriginManager manages origin pulls for streams that have subscribers but no publisher.
type OriginManager struct {
	hub         *core.StreamHub
	eventBus    *core.EventBus
	scheduler   *Scheduler
	registry    *TransportRegistry
	retryMax    int
	retryDelay  time.Duration
	idleTimeout time.Duration

	mu     sync.Mutex
	active map[string]*OriginPull
	closed chan struct{}
}

// NewOriginManager creates a new origin manager.
func NewOriginManager(hub *core.StreamHub, bus *core.EventBus, scheduler *Scheduler, registry *TransportRegistry, retryMax int, retryDelay, idleTimeout time.Duration) *OriginManager {
	if retryMax <= 0 {
		retryMax = 3
	}
	if retryDelay <= 0 {
		retryDelay = 2 * time.Second
	}
	if idleTimeout <= 0 {
		idleTimeout = 30 * time.Second
	}
	return &OriginManager{
		hub:         hub,
		eventBus:    bus,
		scheduler:   scheduler,
		registry:    registry,
		retryMax:    retryMax,
		retryDelay:  retryDelay,
		idleTimeout: idleTimeout,
		active:      make(map[string]*OriginPull),
		closed:      make(chan struct{}),
	}
}

// Hooks returns event hooks for the origin manager.
func (om *OriginManager) Hooks() []core.HookRegistration {
	return []core.HookRegistration{
		{
			Event:    core.EventSubscribe,
			Mode:     core.HookAsync,
			Priority: 100,
			Handler:  om.onSubscribe,
		},
	}
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

	op := NewOriginPull(ctx.StreamKey, servers, stream, om.registry, om.retryMax, om.retryDelay, om.idleTimeout)
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
	close(om.closed)

	om.mu.Lock()
	defer om.mu.Unlock()

	for key, op := range om.active {
		op.Close()
		delete(om.active, key)
	}
}

// ActiveCount returns the number of active origin pulls.
func (om *OriginManager) ActiveCount() int {
	om.mu.Lock()
	defer om.mu.Unlock()
	return len(om.active)
}
