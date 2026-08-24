package core

import configruntime "github.com/im-pingo/liveforge/config/runtime"

// EventType identifies a lifecycle or media event.
type EventType uint16

const (
	EventStreamCreate EventType = iota + 1
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
	HookSync HookMode = iota + 1
	HookAsync
)

// EventContext carries event data passed to hook handlers.
type EventContext struct {
	StreamKey    string
	PublisherID  string
	SubscriberID string
	Protocol     string
	RemoteAddr   string
	Params       map[string]string // URL query params (e.g. "token" -> "xxx")
	Extra        map[string]any
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

// Reloadable is an optional interface modules can implement to support
// config hot-reload via SIGHUP. Only modules whose config has actually
// changed need to do anything in OnReload.
type Reloadable interface {
	OnReload(s *Server) error
}

// ReloadPreparer lets a module validate and construct candidate policy state
// before any reloadable module is mutated. The returned commit must not fail.
type ReloadPreparer interface {
	PrepareReload(s *Server) (commit func(), err error)
}

// ConfigApplied is notified after every reloadable module accepted a runtime
// snapshot and the server committed it.
type ConfigApplied interface {
	OnConfigApplied(snapshot *configruntime.ConfigSnapshot)
}
