package gb28181

import (
	"context"
	"log/slog"

	"github.com/im-pingo/liveforge/core"
	sipmod "github.com/im-pingo/liveforge/module/sip"
	"github.com/im-pingo/liveforge/pkg/portalloc"
)

// Module implements core.Module for GB28181 video surveillance protocol.
type Module struct {
	sipService sipmod.SIPService
	handler    *handler
	registry   *DeviceRegistry
	sessions   *SessionManager
	catalog    *catalogClient
	invite     *inviteClient
	playback   *playbackClient
	alarm      *alarmHandler
	labs       *labManager
}

// NewModule creates a new GB28181 module.
// The SIP service is injected from the SIP module.
func NewModule(sipService sipmod.SIPService) *Module {
	return &Module{
		sipService: sipService,
	}
}

// Name returns the module name.
func (m *Module) Name() string { return "gb28181" }

// Init initializes the GB28181 module: device registry, sessions, SIP handlers, and API.
func (m *Module) Init(s *core.Server) error {
	cfg := s.Config().GB28181

	// Port allocator for RTP
	minPort, maxPort := 40000, 50000
	if len(cfg.RTPPortRange) == 2 {
		minPort, maxPort = cfg.RTPPortRange[0], cfg.RTPPortRange[1]
	}
	ports, err := portalloc.New(minPort, maxPort)
	if err != nil {
		return err
	}

	// Stream prefix
	prefix := cfg.StreamPrefix
	if prefix == "" {
		prefix = "gb28181"
	}

	// Session manager
	m.sessions = NewSessionManager()

	// Device registry
	m.registry = NewDeviceRegistry(cfg.Keepalive.Timeout, cfg.DumpFile)
	m.registry.RestoreFromFile()

	// Auth config
	sipCfg := s.Config().SIP
	var authCfg *sipmod.DigestAuth
	if sipCfg.Auth.Enabled {
		authCfg = sipmod.NewDigestAuth(sipCfg.Domain, sipCfg.Auth.Password)
	}

	// Handler
	m.handler = &handler{
		registry: m.registry,
		sessions: m.sessions,
		hub:      s.StreamHub(),
		bus:      s.GetEventBus(),
		ports:    ports,
		prefix:   prefix,
		auth:     authCfg,
	}
	m.registry.StartMonitor(func(deviceID string) {
		slog.Info("device offline", "module", "gb28181", "device", deviceID)
		m.handler.closeSessionsByDevice(deviceID)
	})

	// Catalog client
	m.catalog = &catalogClient{
		sipService: m.sipService,
	}

	// Invite client
	m.invite = &inviteClient{
		sipService: m.sipService,
		handler:    m.handler,
	}

	// Playback client
	m.playback = &playbackClient{
		sipService: m.sipService,
		handler:    m.handler,
	}

	// Alarm handler
	m.alarm = &alarmHandler{
		registry: m.registry,
	}
	m.labs = newLabManager(m)

	// Register SIP handlers
	m.sipService.OnRegister(m.handler.handleRegister)
	m.sipService.OnInvite(m.handler.handleInvite)
	m.sipService.OnBye(m.handler.handleBye)
	m.sipService.OnMessage(m.handler.handleMessage)
	m.sipService.OnSubscribe(m.alarm.handleSubscribe)

	// Register API handlers
	registerAPI(s, m)

	slog.Info("initialized", "module", "gb28181",
		"prefix", prefix, "rtp_ports", cfg.RTPPortRange)

	return nil
}

// Hooks returns empty hooks — GB28181 uses SIP events, not stream lifecycle hooks.
func (m *Module) Hooks() []core.HookRegistration { return nil }

// StartLabSession starts a persistent in-process fake GB28181 device.
func (m *Module) StartLabSession(ctx context.Context, request LabSessionRequest) (LabSessionSnapshot, error) {
	if m.labs == nil {
		return LabSessionSnapshot{}, ErrLabManagerUnimplemented
	}
	return m.labs.Start(ctx, request)
}

// ListLabSessions returns persistent fake GB28181 device snapshots.
func (m *Module) ListLabSessions() []LabSessionSnapshot {
	if m.labs == nil {
		return []LabSessionSnapshot{}
	}
	return m.labs.List()
}

// StopLabSession stops a persistent fake GB28181 device. Repeated stops are safe.
func (m *Module) StopLabSession(id string) error {
	if m.labs == nil {
		return ErrLabManagerUnimplemented
	}
	return m.labs.Stop(id)
}

// Close stops the GB28181 module.
func (m *Module) Close() error {
	if m.labs != nil {
		m.labs.closeAll()
	}
	if m.registry != nil {
		m.registry.Stop()
	}
	if m.handler != nil && m.sessions != nil {
		for _, session := range m.sessions.All() {
			m.handler.closeSession(session, "")
		}
	}
	slog.Info("stopped", "module", "gb28181")
	return nil
}
