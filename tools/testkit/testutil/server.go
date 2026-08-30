// Package testutil provides helpers for integration tests that need a real
// in-process LiveForge server.
package testutil

import (
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/module/api"
	"github.com/im-pingo/liveforge/module/auth"
	gb28181mod "github.com/im-pingo/liveforge/module/gb28181"
	"github.com/im-pingo/liveforge/module/httpstream"
	"github.com/im-pingo/liveforge/module/rtmp"
	"github.com/im-pingo/liveforge/module/rtsp"
	sipmod "github.com/im-pingo/liveforge/module/sip"
	sipgwmod "github.com/im-pingo/liveforge/module/sipgateway"
	"github.com/im-pingo/liveforge/module/srt"
	"github.com/im-pingo/liveforge/module/webrtc"
	"github.com/im-pingo/liveforge/pkg/avframe"
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

// WithLLHLS enables Low-Latency HLS with the specified container format.
// Valid containers are "fmp4" and "ts". This option also enables HTTP streaming
// if not already enabled.
func WithLLHLS(container string) Option {
	return func(c *config.Config) {
		if !c.HTTP.Enabled {
			c.HTTP.Enabled = true
			c.HTTP.Listen = allocTCPAddr()
		}
		c.HTTP.LLHLS.Enabled = true
		c.HTTP.LLHLS.Container = container
		if c.HTTP.LLHLS.PartDuration == 0 {
			c.HTTP.LLHLS.PartDuration = 0.2
		}
		if c.HTTP.LLHLS.SegmentCount == 0 {
			c.HTTP.LLHLS.SegmentCount = 4
		}
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

// WithAudioCodec enables the optional audio transcoding path for test streams.
func WithAudioCodec() Option {
	return func(c *config.Config) {
		c.AudioCodec.Enabled = true
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
		c.GB28181.Keepalive.Timeout = time.Minute
		c.GB28181.RTPPortRange = allocUDPPortRange(8,
			addressPortRange(c.SIP.Listen), c.SIP.Gateway.RTPPortRange)
	}
}

// WithSIPGateway enables the SIP gateway and its persistent loopback lab.
// Requires WithSIP() to be used as well.
func WithSIPGateway() Option {
	return func(c *config.Config) {
		c.SIP.Gateway.Enabled = true
		c.SIP.Gateway.StreamPrefix = "sip"
		c.SIP.Gateway.Codecs = []string{"PCMA", "PCMU"}
		c.SIP.Gateway.MaxCalls = 8
		c.SIP.Gateway.RTPPortRange = allocUDPPortRange(8,
			addressPortRange(c.SIP.Listen), c.GB28181.RTPPortRange)
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
	if cfg.AudioCodec.Enabled {
		s.StreamHub().SetAudioCodecEnabled(true)
	}

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
	if cfg.SIP.Gateway.Enabled {
		if sipModule == nil {
			t.Fatal("WithSIPGateway requires WithSIP")
		}
		s.RegisterModule(sipgwmod.NewModule(sipModule.Service()))
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

// ModuleByName returns a registered module for integration tests that exercise
// the module's exported control-plane contract.
func (ts *TestServer) ModuleByName(name string) core.Module {
	return ts.server.ModuleByName(name)
}

// StreamHasVideoGOP reports whether a published stream has a decodable video
// start point available for playback integration tests.
func (ts *TestServer) StreamHasVideoGOP(streamKey string) bool {
	stream, ok := ts.server.StreamHub().Find(streamKey)
	return ok && stream.Publisher() != nil && stream.GOPCacheDetail().VideoFrames > 0
}

// StreamHasAudio reports whether the active publisher declares codec and at
// least one audio frame has reached the shared stream hub.
func (ts *TestServer) StreamHasAudio(streamKey string, codec avframe.CodecType) bool {
	stream, ok := ts.server.StreamHub().Find(streamKey)
	if !ok || stream.Publisher() == nil {
		return false
	}
	info := stream.Publisher().MediaInfo()
	return info != nil && info.AudioCodec == codec && stream.Stats().AudioFrames > 0
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
func allocUDPPortRange(pairCount int, excluded ...[]int) []int {
	const (
		minimumPort = 35000
		maximumPort = 59999
	)
	portCount := pairCount * 2
	loopback := net.ParseIP("127.0.0.1")
	for start := minimumPort; start+portCount-1 <= maximumPort; start += 2 {
		end := start + portCount - 1
		overlaps := false
		for _, other := range excluded {
			if len(other) == 2 && start <= other[1] && other[0] <= end {
				overlaps = true
				break
			}
		}
		if overlaps {
			continue
		}

		reservations := make([]*net.UDPConn, 0, portCount)
		available := true
		for port := start; port <= end; port++ {
			conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: loopback, Port: port})
			if err != nil {
				available = false
				break
			}
			reservations = append(reservations, conn)
		}
		for _, conn := range reservations {
			_ = conn.Close()
		}
		if available {
			return []int{start, end}
		}
	}
	panic("allocUDPPortRange: no contiguous loopback UDP range available")
}

func addressPortRange(address string) []int {
	_, portText, err := net.SplitHostPort(address)
	if err != nil {
		return nil
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port <= 0 {
		return nil
	}
	return []int{port, port}
}
