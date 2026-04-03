package cluster

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/im-pingo/liveforge/core"
)

// ForwardTarget manages forwarding a single stream to a single target.
type ForwardTarget struct {
	streamKey  string
	targetURL  string
	stream     *core.Stream
	transport  RelayTransport
	retryMax   int
	retryDelay time.Duration

	mu     sync.Mutex
	closed chan struct{}
}

// NewForwardTarget creates a new forward target.
func NewForwardTarget(streamKey, targetURL string, stream *core.Stream, transport RelayTransport, retryMax int, retryDelay time.Duration) *ForwardTarget {
	return &ForwardTarget{
		streamKey:  streamKey,
		targetURL:  targetURL,
		stream:     stream,
		transport:  transport,
		retryMax:   retryMax,
		retryDelay: retryDelay,
		closed:     make(chan struct{}),
	}
}

// Run starts forwarding frames to the target server.
// It reconnects on failure up to retryMax times with exponential backoff.
func (ft *ForwardTarget) Run() {
	defer slog.Info("forward target stopped", "module", "cluster",
		"stream", ft.streamKey, "target", ft.targetURL)

	for attempt := 0; ; attempt++ {
		select {
		case <-ft.closed:
			return
		default:
		}

		if ft.retryMax > 0 && attempt >= ft.retryMax {
			slog.Warn("forward max retries exceeded", "module", "cluster",
				"stream", ft.streamKey, "target", ft.targetURL, "attempts", attempt)
			return
		}

		if attempt > 0 {
			delay := ft.retryDelay * time.Duration(1<<min(attempt-1, 4))
			if delay > 30*time.Second {
				delay = 30 * time.Second
			}
			slog.Info("forward reconnecting", "module", "cluster",
				"stream", ft.streamKey, "target", ft.targetURL, "attempt", attempt)
			select {
			case <-ft.closed:
				return
			case <-time.After(delay):
			}
		}

		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			select {
			case <-ft.closed:
				cancel()
			case <-ctx.Done():
			}
		}()

		err := ft.transport.Push(ctx, ft.targetURL, ft.stream)
		cancel()

		if err != nil {
			if errors.Is(err, ErrCodecMismatch) {
				slog.Warn("forward codec mismatch, not retrying", "module", "cluster",
					"stream", ft.streamKey, "target", ft.targetURL, "error", err)
				return
			}
			slog.Warn("forward connection error", "module", "cluster",
				"stream", ft.streamKey, "target", ft.targetURL, "error", err)
		}
	}
}

// Close stops the forward target.
func (ft *ForwardTarget) Close() {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	select {
	case <-ft.closed:
	default:
		close(ft.closed)
	}
}

// ForwardManager manages all forward targets for all streams.
type ForwardManager struct {
	hub       *core.StreamHub
	eventBus  *core.EventBus
	scheduler *Scheduler
	registry  *TransportRegistry
	retryMax  int
	retryDel  time.Duration

	mu     sync.Mutex
	active map[string][]*ForwardTarget // streamKey -> targets
	closed chan struct{}
}

// NewForwardManager creates a new forward manager.
func NewForwardManager(hub *core.StreamHub, bus *core.EventBus, scheduler *Scheduler, registry *TransportRegistry, retryMax int, retryDelay time.Duration) *ForwardManager {
	if retryMax <= 0 {
		retryMax = 3
	}
	if retryDelay <= 0 {
		retryDelay = 5 * time.Second
	}
	return &ForwardManager{
		hub:       hub,
		eventBus:  bus,
		scheduler: scheduler,
		registry:  registry,
		retryMax:  retryMax,
		retryDel:  retryDelay,
		active:    make(map[string][]*ForwardTarget),
		closed:    make(chan struct{}),
	}
}

// Hooks returns event hooks for the forward manager.
func (fm *ForwardManager) Hooks() []core.HookRegistration {
	return []core.HookRegistration{
		{
			Event:    core.EventPublish,
			Mode:     core.HookAsync,
			Priority: 100,
			Handler:  fm.onPublish,
		},
		{
			Event:    core.EventPublishStop,
			Mode:     core.HookAsync,
			Priority: 100,
			Handler:  fm.onPublishStop,
		},
	}
}

func (fm *ForwardManager) onPublish(ctx *core.EventContext) error {
	stream, ok := fm.hub.Find(ctx.StreamKey)
	if !ok {
		return nil
	}

	fm.mu.Lock()
	defer fm.mu.Unlock()

	if _, exists := fm.active[ctx.StreamKey]; exists {
		return nil
	}

	targets, err := fm.scheduler.Resolve("forward", ctx.StreamKey)
	if err != nil {
		slog.Warn("forward schedule resolve failed", "module", "cluster",
			"stream", ctx.StreamKey, "error", err)
		return nil
	}

	// Extract stream name from key (e.g. "live/cluster_test" → "cluster_test").
	// Target URLs in config are base URLs (e.g. "rtmp://host/app"),
	// so we append the stream name to form the full URL.
	streamName := ctx.StreamKey
	if idx := strings.Index(ctx.StreamKey, "/"); idx >= 0 {
		streamName = ctx.StreamKey[idx+1:]
	}

	var fts []*ForwardTarget
	for _, targetURL := range targets {
		fullURL := strings.TrimRight(targetURL, "/") + "/" + streamName
		transport, err := fm.registry.Resolve(fullURL)
		if err != nil {
			slog.Warn("unsupported relay protocol", "module", "cluster",
				"url", fullURL, "error", err)
			continue
		}

		ft := NewForwardTarget(ctx.StreamKey, fullURL, stream, transport, fm.retryMax, fm.retryDel)
		fts = append(fts, ft)
		go ft.Run()
	}

	if len(fts) > 0 {
		fm.active[ctx.StreamKey] = fts
		fm.eventBus.Emit(core.EventForwardStart, &core.EventContext{
			StreamKey: ctx.StreamKey,
			Extra:     map[string]any{"target_count": len(fts)},
		})
	}

	return nil
}

func (fm *ForwardManager) onPublishStop(ctx *core.EventContext) error {
	fm.mu.Lock()
	fts, ok := fm.active[ctx.StreamKey]
	delete(fm.active, ctx.StreamKey)
	fm.mu.Unlock()

	if ok {
		for _, ft := range fts {
			ft.Close()
		}
		fm.eventBus.Emit(core.EventForwardStop, &core.EventContext{ //nolint:errcheck
			StreamKey: ctx.StreamKey,
		})
	}

	return nil
}

// Close stops all active forward targets.
func (fm *ForwardManager) Close() {
	close(fm.closed)

	fm.mu.Lock()
	defer fm.mu.Unlock()

	for key, fts := range fm.active {
		for _, ft := range fts {
			ft.Close()
		}
		delete(fm.active, key)
	}
}

// ActiveCount returns the number of streams being forwarded.
func (fm *ForwardManager) ActiveCount() int {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	return len(fm.active)
}
