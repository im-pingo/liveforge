package cluster

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/module/rtmp"
	"github.com/im-pingo/liveforge/pkg/avframe"
)

// OriginPull manages pulling a single stream from an origin server.
type OriginPull struct {
	streamKey   string
	servers     []string
	stream      *core.Stream
	retryMax    int
	retryDelay  time.Duration
	idleTimeout time.Duration

	mu     sync.Mutex
	closed chan struct{}
}

// NewOriginPull creates a new origin pull instance.
func NewOriginPull(streamKey string, servers []string, stream *core.Stream, retryMax int, retryDelay, idleTimeout time.Duration) *OriginPull {
	return &OriginPull{
		streamKey:   streamKey,
		servers:     servers,
		stream:      stream,
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
func (op *OriginPull) pullOnce(targetURL string) error {
	host, app, streamName, err := parseRTMPURL(targetURL)
	if err != nil {
		return fmt.Errorf("parse URL: %w", err)
	}

	rc, err := dialRTMP(host)
	if err != nil {
		return err
	}
	defer rc.conn.Close()

	// Connect
	if err := rc.sendConnect(app); err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	if err := rc.readResponses(1); err != nil {
		return fmt.Errorf("connect response: %w", err)
	}

	// Play sequence: createStream
	if err := rc.sendPlay(streamName); err != nil {
		return fmt.Errorf("play commands: %w", err)
	}
	if err := rc.readResponses(2); err != nil {
		return fmt.Errorf("createStream response: %w", err)
	}

	// Send play command on stream ID 1
	playPayload, _ := rtmp.AMF0Encode("play", float64(3), nil, streamName)
	if err := rc.cw.WriteMessage(8, &rtmp.Message{
		TypeID:   rtmp.MsgAMF0Command,
		Length:   uint32(len(playPayload)),
		StreamID: 1,
		Payload:  playPayload,
	}); err != nil {
		return fmt.Errorf("play: %w", err)
	}

	slog.Info("origin pull connected", "module", "cluster",
		"stream", op.streamKey, "url", targetURL)

	// Set up publisher for the local stream
	pub := &originPublisher{
		id:   fmt.Sprintf("origin-pull-%s", op.streamKey),
		info: &avframe.MediaInfo{},
	}
	if err := op.stream.SetPublisher(pub); err != nil {
		return fmt.Errorf("set publisher: %w", err)
	}
	defer op.stream.RemovePublisher()

	// Read media messages from the origin
	for {
		select {
		case <-op.closed:
			return nil
		default:
		}

		msg, err := rc.cr.ReadMessage()
		if err != nil {
			return fmt.Errorf("read message: %w", err)
		}

		switch msg.TypeID {
		case rtmp.MsgSetChunkSize:
			if len(msg.Payload) >= 4 {
				size := int(msg.Payload[0])<<24 | int(msg.Payload[1])<<16 | int(msg.Payload[2])<<8 | int(msg.Payload[3])
				rc.cr.SetChunkSize(size)
			}

		case rtmp.MsgVideo:
			frame := parseVideoPayload(msg.Payload, int64(msg.Timestamp))
			if frame != nil {
				// Update media info on first video frame
				if pub.info.VideoCodec == 0 {
					pub.info.VideoCodec = frame.Codec
				}
				op.stream.WriteFrame(frame)
			}

		case rtmp.MsgAudio:
			frame := parseAudioPayload(msg.Payload, int64(msg.Timestamp))
			if frame != nil {
				if pub.info.AudioCodec == 0 {
					pub.info.AudioCodec = frame.Codec
				}
				op.stream.WriteFrame(frame)
			}

		case rtmp.MsgAMF0Command:
			vals, err := rtmp.AMF0Decode(msg.Payload)
			if err != nil || len(vals) < 1 {
				continue
			}
			cmd, _ := vals[0].(string)
			if cmd == "onStatus" && len(vals) >= 4 {
				if m, ok := vals[3].(map[string]any); ok {
					code, _ := m["code"].(string)
					if code == "NetStream.Play.UnpublishNotify" || code == "NetStream.Play.Stop" {
						slog.Info("origin stream ended", "module", "cluster",
							"stream", op.streamKey, "code", code)
						return nil
					}
				}
			}
		}
	}
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
	retryMax    int
	retryDelay  time.Duration
	idleTimeout time.Duration

	mu     sync.Mutex
	active map[string]*OriginPull
	closed chan struct{}
}

// NewOriginManager creates a new origin manager.
func NewOriginManager(hub *core.StreamHub, bus *core.EventBus, scheduler *Scheduler, retryMax int, retryDelay, idleTimeout time.Duration) *OriginManager {
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

	op := NewOriginPull(ctx.StreamKey, servers, stream, om.retryMax, om.retryDelay, om.idleTimeout)
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
