package core

// EventType identifies a lifecycle or media event.
type EventType uint16

const (
	EventStreamCreate    EventType = iota + 1
	EventStreamDestroy
	EventPublish
	EventPublishStop
	EventRepublish
	EventSubscribe
	EventSubscribeStop
	EventPublishAlive
	EventSubscribeAlive
	EventStreamAlive
	EventVideoKeyframe
	EventAudioHeader
	EventForwardStart
	EventForwardStop
	EventOriginPullStart
	EventOriginPullStop
	EventSubscriberSkip
)

// HookMode determines whether a hook runs synchronously or asynchronously.
type HookMode uint8

const (
	HookSync  HookMode = iota + 1
	HookAsync
)

// EventContext carries event data passed to hook handlers.
type EventContext struct {
	StreamKey  string
	Protocol   string
	RemoteAddr string
	Extra      map[string]any
}

// EventHandler is a function that handles an event.
type EventHandler func(ctx *EventContext) error

// HookRegistration describes a handler bound to an event.
type HookRegistration struct {
	Event    EventType
	Mode     HookMode
	Priority int // lower = higher priority, executed first
	Handler  EventHandler
}

// Module is the interface all server modules must implement.
type Module interface {
	Name() string
	Init(s *Server) error
	Hooks() []HookRegistration
	Close() error
}
