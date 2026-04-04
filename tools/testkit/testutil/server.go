// Package testutil provides helpers for integration tests that need a real
// in-process LiveForge server.
package testutil

import (
	"net"
	"sync"
	"testing"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/module/api"
	"github.com/im-pingo/liveforge/module/auth"
	gb28181mod "github.com/im-pingo/liveforge/module/gb28181"
	"github.com/im-pingo/liveforge/module/httpstream"
	"github.com/im-pingo/liveforge/module/rtmp"
	"github.com/im-pingo/liveforge/module/rtsp"
	sipmod "github.com/im-pingo/liveforge/module/sip"
	"github.com/im-pingo/liveforge/module/srt"
	"github.com/im-pingo/liveforge/module/webrtc"
)

// Option configures the test server's Config before startup.
type Option func(*config.Config)

// WithRTMP enables the RTMP module on an auto-allocated port.
func WithRTMP() Option {
	return func(c *config.Config) {
		c.RTMP.Enabled = true
		c.RTMP.Listen = allocTCPAddr()
		if c.RTMP.ChunkSize == 0 {
			c.RTMP.ChunkSize = 4096
		}
	}
}

// WithRTSP enables the RTSP module on an auto-allocated port.
func WithRTSP() Option {
	return func(c *config.Config) {
		c.RTSP.Enabled = true
		c.RTSP.Listen = allocTCPAddr()
	}
}

// WithSRT enables the SRT module on an auto-allocated port.
// SRT uses UDP internally, so the port is allocated via UDP.
func WithSRT() Option {
	return func(c *config.Config) {
		c.SRT.Enabled = true
		c.SRT.Listen = allocUDPAddr()
	}
}

// WithWebRTC enables the WebRTC module (WHIP/WHEP HTTP signaling) on an
// auto-allocated port.
func WithWebRTC() Option {
	return func(c *config.Config) {
		c.WebRTC.Enabled = true
		c.WebRTC.Listen = allocTCPAddr()
		c.WebRTC.ICELite = true
	}
}

// WithHTTPStream enables the HTTP streaming module (FLV/TS/FMP4/HLS/DASH) on
// an auto-allocated port.
func WithHTTPStream() Option {
	return func(c *config.Config) {
		c.HTTP.Enabled = true
		c.HTTP.Listen = allocTCPAddr()
	}
}

// WithAPI enables the management API module on an auto-allocated port.
func WithAPI() Option {
	return func(c *config.Config) {
		c.API.Enabled = true
		c.API.Listen = allocTCPAddr()
	}
}

// WithAuth enables token-based authentication for both publish and subscribe.
func WithAuth(secret string) Option {
	return func(c *config.Config) {
		c.Auth.Publish.Mode = "token"
		c.Auth.Publish.Token.Secret = secret
		c.Auth.Subscribe.Mode = "token"
		c.Auth.Subscribe.Token.Secret = secret
	}
}

// WithSIP enables the SIP module on an auto-allocated UDP port.
func WithSIP() Option {
	return func(c *config.Config) {
		c.SIP.Enabled = true
		c.SIP.Listen = allocUDPAddr()
		c.SIP.Transport = []string{"udp"}
		c.SIP.ServerID = "34020000002000000001"
		c.SIP.Domain = "3402000000"
	}
}

// WithGB28181 enables the GB28181 module with auto-allocated RTP ports.
// Requires WithSIP() to be used as well.
func WithGB28181() Option {
	return func(c *config.Config) {
		c.GB28181.Enabled = true
		c.GB28181.StreamPrefix = "gb28181"
		// Allocate a dynamic base port, clamped to avoid overflow.
		base := allocUDPPortPair()
		c.GB28181.RTPPortRange = []int{base, base + 100}
	}
}



// TestServer wraps a running LiveForge server for integration testing.
type TestServer struct {
	server *core.Server
	cfg    *config.Config

	shutdownOnce sync.Once
}

// StartTestServer creates and starts a LiveForge server configured via the
// supplied options. Each option typically enables one protocol module with an
// auto-allocated listen address. The server is automatically shut down when the
// test completes via t.Cleanup.
func StartTestServer(t *testing.T, opts ...Option) *TestServer {
	t.Helper()

	cfg := defaultConfig()
	for _, opt := range opts {
		opt(cfg)
	}

	s := core.NewServer(cfg)

	// Register modules based on what the options enabled.
	// Order matters: modules that register API handlers (e.g. GB28181) must
	// be registered before the API module so their handlers are available when
	// the API mux is built during Init.
	if cfg.RTMP.Enabled {
		s.RegisterModule(rtmp.NewModule())
	}
	if cfg.RTSP.Enabled {
		s.RegisterModule(rtsp.NewModule())
	}
	if cfg.SRT.Enabled {
		s.RegisterModule(srt.NewModule())
	}
	if cfg.WebRTC.Enabled {
		s.RegisterModule(webrtc.NewModule())
	}
	if cfg.HTTP.Enabled {
		s.RegisterModule(httpstream.NewModule())
	}
	if cfg.Auth.Publish.Mode != "" || cfg.Auth.Subscribe.Mode != "" {
		s.RegisterModule(auth.NewModule())
	}

	// SIP must be registered before GB28181 because GB28181 needs the SIP service.
	var sipModule *sipmod.Module
	if cfg.SIP.Enabled {
		sipModule = sipmod.NewModule()
		s.RegisterModule(sipModule)
	}
	if cfg.GB28181.Enabled {
		if sipModule == nil {
			t.Fatal("WithGB28181 requires WithSIP")
		}
		s.RegisterModule(gb28181mod.NewModule(sipModule.Service()))
	}

	// API module must be registered last so cross-module handlers are available.
	if cfg.API.Enabled {
		s.RegisterModule(api.NewModule())
	}

	if err := s.Init(); err != nil {
		t.Fatalf("StartTestServer: Init failed: %v", err)
	}

	ts := &TestServer{server: s, cfg: cfg}
	t.Cleanup(ts.Shutdown)

	return ts
}

// RTMPAddr returns the RTMP listen address, or "" if RTMP is not enabled.
func (ts *TestServer) RTMPAddr() string {
	if !ts.cfg.RTMP.Enabled {
		return ""
	}
	return ts.cfg.RTMP.Listen
}

// RTSPAddr returns the RTSP listen address, or "" if RTSP is not enabled.
func (ts *TestServer) RTSPAddr() string {
	if !ts.cfg.RTSP.Enabled {
		return ""
	}
	return ts.cfg.RTSP.Listen
}

// SRTAddr returns the SRT listen address, or "" if SRT is not enabled.
func (ts *TestServer) SRTAddr() string {
	if !ts.cfg.SRT.Enabled {
		return ""
	}
	return ts.cfg.SRT.Listen
}

// WebRTCAddr returns the WebRTC HTTP signaling address, or "" if WebRTC is not
// enabled.
func (ts *TestServer) WebRTCAddr() string {
	if !ts.cfg.WebRTC.Enabled {
		return ""
	}
	return ts.cfg.WebRTC.Listen
}

// HTTPAddr returns the HTTP streaming listen address, or "" if HTTP streaming
// is not enabled.
func (ts *TestServer) HTTPAddr() string {
	if !ts.cfg.HTTP.Enabled {
		return ""
	}
	return ts.cfg.HTTP.Listen
}

// APIAddr returns the API listen address, or "" if the API is not enabled.
func (ts *TestServer) APIAddr() string {
	if !ts.cfg.API.Enabled {
		return ""
	}
	return ts.cfg.API.Listen
}

// SIPAddr returns the SIP listen address, or "" if SIP is not enabled.
func (ts *TestServer) SIPAddr() string {
	if !ts.cfg.SIP.Enabled {
		return ""
	}
	return ts.cfg.SIP.Listen
}

// Config returns the server configuration.
func (ts *TestServer) Config() *config.Config {
	return ts.cfg
}

// Shutdown stops the server. It is safe to call multiple times; only the first
// call performs the actual shutdown.
func (ts *TestServer) Shutdown() {
	ts.shutdownOnce.Do(func() {
		ts.server.Shutdown()
	})
}

// defaultConfig returns a minimal Config with all modules disabled and sensible
// defaults for fields that the server/modules require.
func defaultConfig() *config.Config {
	return &config.Config{
		Server: config.ServerConfig{
			Name:     "liveforge-test",
			LogLevel: "error",
		},
		Stream: config.StreamConfig{
			GOPCache:       true,
			GOPCacheNum:    1,
			RingBufferSize: 1024,
		},
	}
}

// allocTCPAddr binds a TCP listener on 127.0.0.1:0, captures the
// kernel-assigned address, then closes the listener. The returned address is in
// "host:port" form and is free to be reused immediately.
func allocTCPAddr() string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic("allocTCPAddr: " + err.Error())
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

// allocUDPAddr binds a UDP socket on 127.0.0.1:0, captures the
// kernel-assigned address, then closes the socket. This is used for protocols
// that listen on UDP (e.g., SRT).
func allocUDPAddr() string {
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		panic("allocUDPAddr: " + err.Error())
	}
	addr := conn.LocalAddr().String()
	conn.Close()
	return addr
}

// allocUDPPortPair allocates a free even-numbered UDP port suitable as the
// base of an RTP port range. The result is clamped so that base+100 stays
// within the valid port space.
func allocUDPPortPair() int {
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		panic("allocUDPPortPair: " + err.Error())
	}
	port := conn.LocalAddr().(*net.UDPAddr).Port
	conn.Close()
	if port%2 != 0 {
		port++
	}
	if port+100 > 65535 {
		port = 65400 // safe fallback
	}
	return port
}
