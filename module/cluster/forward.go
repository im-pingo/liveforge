package cluster

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/module/rtmp"
)

// ForwardTarget manages forwarding a single stream to a single RTMP target.
type ForwardTarget struct {
	streamKey  string
	targetURL  string
	stream     *core.Stream
	retryMax   int
	retryDelay time.Duration

	mu     sync.Mutex
	closed chan struct{}
}

// NewForwardTarget creates a new forward target.
func NewForwardTarget(streamKey, targetURL string, stream *core.Stream, retryMax int, retryDelay time.Duration) *ForwardTarget {
	return &ForwardTarget{
		streamKey:  streamKey,
		targetURL:  targetURL,
		stream:     stream,
		retryMax:   retryMax,
		retryDelay: retryDelay,
		closed:     make(chan struct{}),
	}
}

// Run starts forwarding frames to the target RTMP server.
// It reconnects on failure up to retryMax times.
func (ft *ForwardTarget) Run() {
	defer slog.Info("forward target stopped", "module", "cluster", "stream", ft.streamKey, "target", ft.targetURL)

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
			slog.Info("forward reconnecting", "module", "cluster",
				"stream", ft.streamKey, "target", ft.targetURL, "attempt", attempt)
			select {
			case <-ft.closed:
				return
			case <-time.After(ft.retryDelay):
			}
		}

		err := ft.forwardOnce()
		if err != nil {
			slog.Warn("forward connection error", "module", "cluster",
				"stream", ft.streamKey, "target", ft.targetURL, "error", err)
		}
	}
}

// forwardOnce establishes one RTMP connection, publishes, and streams frames until
// the connection fails or the target is closed.
func (ft *ForwardTarget) forwardOnce() error {
	host, app, streamName, err := parseRTMPURL(ft.targetURL)
	if err != nil {
		return fmt.Errorf("parse URL: %w", err)
	}

	rc, err := dialRTMP(host)
	if err != nil {
		return err
	}
	defer rc.conn.Close()

	// Set chunk size
	if err := rc.setChunkSize(defaultChunkSize); err != nil {
		return fmt.Errorf("set chunk size: %w", err)
	}

	// Connect
	if err := rc.sendConnect(app); err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	if err := rc.readResponses(1); err != nil {
		return fmt.Errorf("connect response: %w", err)
	}

	// Publish sequence
	if err := rc.sendPublish(streamName); err != nil {
		return fmt.Errorf("publish commands: %w", err)
	}
	// Wait for createStream result (txn 4)
	if err := rc.readResponses(4); err != nil {
		return fmt.Errorf("createStream response: %w", err)
	}

	// Send actual publish command on stream ID 1
	publishPayload, _ := rtmp.AMF0Encode("publish", float64(5), nil, streamName, "live")
	if err := rc.cw.WriteMessage(8, &rtmp.Message{
		TypeID:   rtmp.MsgAMF0Command,
		Length:   uint32(len(publishPayload)),
		StreamID: 1,
		Payload:  publishPayload,
	}); err != nil {
		return fmt.Errorf("publish: %w", err)
	}

	// Read onStatus for publish
	if err := rc.readResponses(5); err != nil {
		return fmt.Errorf("publish response: %w", err)
	}

	slog.Info("forward connected", "module", "cluster",
		"stream", ft.streamKey, "target", ft.targetURL)

	// Send sequence headers first
	if vsh := ft.stream.VideoSeqHeader(); vsh != nil {
		if err := rc.sendMediaFrame(vsh); err != nil {
			return fmt.Errorf("video seq header: %w", err)
		}
	}
	if ash := ft.stream.AudioSeqHeader(); ash != nil {
		if err := rc.sendMediaFrame(ash); err != nil {
			return fmt.Errorf("audio seq header: %w", err)
		}
	}

	// Read from ring buffer and forward
	reader := ft.stream.RingBuffer().NewReader()
	for {
		select {
		case <-ft.closed:
			return nil
		default:
		}

		frame, ok := reader.TryRead()
		if !ok {
			if ft.stream.RingBuffer().IsClosed() {
				return nil
			}
			// Wait for new data
			select {
			case <-ft.closed:
				return nil
			case <-ft.stream.RingBuffer().Signal():
			}
			continue
		}

		if err := rc.sendMediaFrame(frame); err != nil {
			return fmt.Errorf("send frame: %w", err)
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
	hub      *core.StreamHub
	eventBus *core.EventBus
	targets  []string
	retryMax int
	retryDel time.Duration

	mu      sync.Mutex
	active  map[string][]*ForwardTarget // streamKey -> targets
	closed  chan struct{}
}

// NewForwardManager creates a new forward manager.
func NewForwardManager(hub *core.StreamHub, bus *core.EventBus, targets []string, retryMax int, retryDelay time.Duration) *ForwardManager {
	if retryMax <= 0 {
		retryMax = 3
	}
	if retryDelay <= 0 {
		retryDelay = 5 * time.Second
	}
	return &ForwardManager{
		hub:      hub,
		eventBus: bus,
		targets:  targets,
		retryMax: retryMax,
		retryDel: retryDelay,
		active:   make(map[string][]*ForwardTarget),
		closed:   make(chan struct{}),
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

	// Don't create duplicates
	if _, exists := fm.active[ctx.StreamKey]; exists {
		return nil
	}

	var fts []*ForwardTarget
	for _, target := range fm.targets {
		// Build full target URL: target base + stream key
		url := target
		if !containsStreamPath(target) {
			url = fmt.Sprintf("%s/%s", target, ctx.StreamKey)
		}

		ft := NewForwardTarget(ctx.StreamKey, url, stream, fm.retryMax, fm.retryDel)
		fts = append(fts, ft)
		go ft.Run()
	}

	fm.active[ctx.StreamKey] = fts

	fm.eventBus.Emit(core.EventForwardStart, &core.EventContext{ //nolint:errcheck
		StreamKey: ctx.StreamKey,
		Extra:     map[string]any{"target_count": len(fts)},
	})

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

// containsStreamPath checks if a target URL already contains a stream path
// beyond the app name (i.e., has more than one path segment after the host).
func containsStreamPath(url string) bool {
	// Count path segments: rtmp://host/app/stream has 2 segments
	stripped := url
	if idx := findSchemeEnd(stripped); idx >= 0 {
		stripped = stripped[idx:]
	}
	// Find path start
	slashIdx := 0
	for i, c := range stripped {
		if c == '/' {
			slashIdx = i
			break
		}
	}
	path := stripped[slashIdx+1:]
	parts := 0
	for _, p := range splitPath(path) {
		if p != "" {
			parts++
		}
	}
	return parts >= 2
}

func findSchemeEnd(s string) int {
	idx := 0
	for i := range s {
		if s[i] == '/' && i > 0 && s[i-1] == '/' {
			idx = i + 1
			break
		}
	}
	return idx
}

func splitPath(s string) []string {
	var parts []string
	start := 0
	for i := range s {
		if s[i] == '/' {
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	parts = append(parts, s[start:])
	return parts
}
