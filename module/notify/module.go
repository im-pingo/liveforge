package notify

import (
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
)

// eventMapping maps EventType to webhook event name.
var eventMapping = map[core.EventType]string{
	core.EventPublish:        "on_publish",
	core.EventPublishStop:    "on_publish_stop",
	core.EventSubscribe:      "on_subscribe",
	core.EventSubscribeStop:  "on_subscribe_stop",
	core.EventStreamCreate:   "on_stream_create",
	core.EventStreamDestroy:  "on_stream_destroy",
	core.EventPublishAlive:   "on_publish_alive",
	core.EventSubscribeAlive: "on_subscribe_alive",
	core.EventStreamAlive:    "on_stream_alive",
}

// Module implements HTTP and WebSocket notifications for stream lifecycle events.
type Module struct {
	cfg          config.NotifyConfig
	sender       *HTTPSender
	senderMu     sync.RWMutex
	wsSender     *WSSender
	drainTimeout time.Duration
}

// NewModule creates a new notify module.
func NewModule() *Module {
	return &Module{}
}

// Name returns the module name.
func (m *Module) Name() string { return "notify" }

// Init reads config and starts the HTTP and WebSocket senders.
func (m *Module) Init(s *core.Server) error {
	m.cfg = s.Config().Notify
	m.drainTimeout = s.Config().Server.DrainTimeout
	if m.cfg.HTTP.Enabled && len(m.cfg.HTTP.Endpoints) > 0 {
		m.sender = NewHTTPSender(m.cfg.HTTP.Endpoints)
		m.sender.Start()
	}
	if m.cfg.WebSocket.Enabled {
		m.wsSender = NewWSSender()
		path := m.cfg.WebSocket.Path
		if path == "" {
			path = "/api/v1/events"
		}
		s.RegisterAPIHandler(path, http.HandlerFunc(m.wsSender.HandleWebSocket))
	}
	slog.Info("enabled", "module", "notify", "http_endpoints", len(m.cfg.HTTP.Endpoints), "websocket", m.cfg.WebSocket.Enabled)
	return nil
}

// OnReload applies webhook endpoint, timeout, secret, retry, and event-filter
// policy to subsequent deliveries. WebSocket enablement/path changes still
// require restart because they alter route registration.
func (m *Module) OnReload(s *core.Server) error {
	next := s.Config().Notify
	m.cfg = next
	m.senderMu.Lock()
	m.drainTimeout = s.Config().Server.DrainTimeout
	sender := m.sender
	created := false
	if sender == nil && next.HTTP.Enabled && len(next.HTTP.Endpoints) > 0 {
		sender = NewHTTPSender(next.HTTP.Endpoints)
		m.sender = sender
		created = true
	}
	m.senderMu.Unlock()
	if sender != nil {
		sender.UpdateEndpoints(next.HTTP.Endpoints)
		if created {
			sender.Start()
		}
	}
	return nil
}

// Hooks returns async hooks for all lifecycle events at priority 90.
func (m *Module) Hooks() []core.HookRegistration {
	var hooks []core.HookRegistration
	for eventType, eventName := range eventMapping {
		hooks = append(hooks, core.HookRegistration{
			Event:    eventType,
			Mode:     core.HookAsync,
			Priority: 90,
			Consumer: "notify",
			Handler:  m.onEvent(eventName),
		})
	}
	return hooks
}

// Close stops the HTTP and WebSocket senders.
func (m *Module) Close() error {
	m.senderMu.Lock()
	sender := m.sender
	drainTimeout := m.drainTimeout
	m.sender = nil
	m.senderMu.Unlock()
	if sender != nil {
		if !sender.StopWithTimeout(drainTimeout) {
			slog.Warn("HTTP notification drain timed out", "module", "notify", "timeout", drainTimeout)
		}
	}
	if m.wsSender != nil {
		m.wsSender.Close()
	}
	slog.Info("stopped", "module", "notify")
	return nil
}

func (m *Module) onEvent(eventName string) core.EventHandler {
	return func(ctx *core.EventContext) error {
		payload := BuildPayload(eventName, ctx)
		m.senderMu.RLock()
		sender := m.sender
		m.senderMu.RUnlock()
		if sender != nil {
			sender.Send(payload)
		}
		if m.wsSender != nil {
			m.wsSender.Send(payload)
		}
		return nil
	}
}
