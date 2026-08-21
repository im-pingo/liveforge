package core

import "github.com/im-pingo/liveforge/config"

// RuntimeConfig is an immutable view of one atomically published server
// configuration. Its private backing pointer cannot be mutated by callers.
type RuntimeConfig struct {
	cfg *config.Config
}

// RuntimeAPIConfig contains only the API fields read on request paths. It has
// no reference-bearing fields and is safe to return by value.
type RuntimeAPIConfig struct {
	PprofEnabled bool
	Auth         config.APIAuthConfig
	Console      config.ConsoleConfig
}

// RuntimeHTTPConfig contains the HTTP streaming fields read on request paths.
type RuntimeHTTPConfig struct {
	CORS  bool
	HLS   config.HLSConfig
	DASH  config.DASHConfig
	LLHLS config.LLHLSConfig
}

// RuntimeEndpoint describes one advertised protocol endpoint.
type RuntimeEndpoint struct {
	Enabled bool
	Listen  string
}

// RuntimeEndpoints contains the endpoint fields exposed by server info.
type RuntimeEndpoints struct {
	HTTP   RuntimeEndpoint
	WebRTC RuntimeEndpoint
	RTMP   RuntimeEndpoint
	RTSP   RuntimeEndpoint
}

// RuntimeSRTConfig contains SRT values read when accepting or subscribing.
type RuntimeSRTConfig struct {
	Passphrase  string
	SkipTracker *config.SkipTrackerConfig
}

// RuntimeRTSPConfig contains RTSP values read when creating subscribers.
type RuntimeRTSPConfig struct {
	SkipTracker *config.SkipTrackerConfig
}

// RuntimeWebRTCICEConfig contains the ICE values read for each peer session.
type RuntimeWebRTCICEConfig struct {
	ICELite    bool
	ICEServers []config.ICEServer
}

// RuntimeConfig returns a zero-allocation view of the current configuration.
func (s *Server) RuntimeConfig() RuntimeConfig {
	return RuntimeConfig{cfg: s.currentConfig()}
}

// Auth returns the media authorization configuration. AuthConfig contains no
// pointer, slice, or map fields, so the returned value cannot alias the view.
func (c RuntimeConfig) Auth() config.AuthConfig {
	return c.cfg.Auth
}

// API returns an alias-free value for API authentication and pprof routing.
func (c RuntimeConfig) API() RuntimeAPIConfig {
	return RuntimeAPIConfig{
		PprofEnabled: c.cfg.API.PprofEnabled,
		Auth:         c.cfg.API.Auth,
		Console:      c.cfg.API.Console,
	}
}

// HTTP returns an alias-free value for HTTP streaming request handling.
func (c RuntimeConfig) HTTP() RuntimeHTTPConfig {
	return RuntimeHTTPConfig{
		CORS: c.cfg.HTTP.CORS, HLS: c.cfg.HTTP.HLS,
		DASH: c.cfg.HTTP.DASH, LLHLS: c.cfg.HTTP.LLHLS,
	}
}

// Endpoints returns the alias-free protocol endpoint settings.
func (c RuntimeConfig) Endpoints() RuntimeEndpoints {
	return RuntimeEndpoints{
		HTTP:   RuntimeEndpoint{Enabled: c.cfg.HTTP.Enabled, Listen: c.cfg.HTTP.Listen},
		WebRTC: RuntimeEndpoint{Enabled: c.cfg.WebRTC.Enabled, Listen: c.cfg.WebRTC.Listen},
		RTMP:   RuntimeEndpoint{Enabled: c.cfg.RTMP.Enabled, Listen: c.cfg.RTMP.Listen},
		RTSP:   RuntimeEndpoint{Enabled: c.cfg.RTSP.Enabled, Listen: c.cfg.RTSP.Listen},
	}
}

// SRT returns only the SRT values needed after initialization. The optional
// skip tracker is copied so callers cannot mutate the published config.
func (c RuntimeConfig) SRT() RuntimeSRTConfig {
	return RuntimeSRTConfig{
		Passphrase:  c.cfg.SRT.Passphrase,
		SkipTracker: cloneSkipTracker(c.cfg.SRT.SkipTracker),
	}
}

// RTSP returns only the RTSP values needed after initialization. The optional
// skip tracker is copied so callers cannot mutate the published config.
func (c RuntimeConfig) RTSP() RuntimeRTSPConfig {
	return RuntimeRTSPConfig{SkipTracker: cloneSkipTracker(c.cfg.RTSP.SkipTracker)}
}

// WebRTCICE returns a targeted deep copy of the ICE settings used when a peer
// session is created, without cloning unrelated server configuration.
func (c RuntimeConfig) WebRTCICE() RuntimeWebRTCICEConfig {
	servers := make([]config.ICEServer, len(c.cfg.WebRTC.ICEServers))
	for i, server := range c.cfg.WebRTC.ICEServers {
		servers[i] = server
		servers[i].URLs = append([]string(nil), server.URLs...)
	}
	return RuntimeWebRTCICEConfig{ICELite: c.cfg.WebRTC.ICELite, ICEServers: servers}
}

func cloneSkipTracker(value *config.SkipTrackerConfig) *config.SkipTrackerConfig {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}
