package notify

import (
	"log"

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

// Module implements HTTP webhook notifications for stream lifecycle events.
type Module struct {
	cfg    config.NotifyConfig
	sender *HTTPSender
}

// NewModule creates a new notify module.
func NewModule() *Module {
	return &Module{}
}

// Name returns the module name.
func (m *Module) Name() string { return "notify" }

// Init reads config and starts the HTTP sender.
func (m *Module) Init(s *core.Server) error {
	m.cfg = s.Config().Notify
	if m.cfg.HTTP.Enabled && len(m.cfg.HTTP.Endpoints) > 0 {
		m.sender = NewHTTPSender(m.cfg.HTTP.Endpoints)
		m.sender.Start()
	}
	log.Printf("[notify] enabled, %d HTTP endpoints", len(m.cfg.HTTP.Endpoints))
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
			Handler:  m.onEvent(eventName),
		})
	}
	return hooks
}

// Close stops the HTTP sender.
func (m *Module) Close() error {
	if m.sender != nil {
		m.sender.Stop()
	}
	log.Println("[notify] stopped")
	return nil
}

func (m *Module) onEvent(eventName string) core.EventHandler {
	return func(ctx *core.EventContext) error {
		payload := BuildPayload(eventName, ctx)
		if m.sender != nil {
			m.sender.Send(payload)
		}
		return nil
	}
}
