package webrtc

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/ratelimit"
	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v4"
)

// Module implements the WebRTC WHIP/WHEP module.
type Module struct {
	server   *core.Server
	api      *webrtc.API
	sessions sync.Map // sessionID -> *Session
	listener net.Listener
	httpSrv  *http.Server
	limiter  *ratelimit.Limiter
	wg       sync.WaitGroup
}

// NewModule creates a new WebRTC module.
func NewModule() *Module {
	return &Module{}
}

// Name returns the module name.
func (m *Module) Name() string { return "webrtc" }

// Init sets up the pion WebRTC API and starts the HTTP signaling server.
func (m *Module) Init(s *core.Server) error {
	m.server = s
	cfg := s.Config()

	// Configure pion SettingEngine.
	se := webrtc.SettingEngine{}

	// Set UDP port range for ICE candidates.
	if len(cfg.WebRTC.UDPPortRange) == 2 {
		se.SetEphemeralUDPPortRange(uint16(cfg.WebRTC.UDPPortRange[0]), uint16(cfg.WebRTC.UDPPortRange[1]))
	}

	// Set external candidate IPs if configured.
	if len(cfg.WebRTC.Candidates) > 0 {
		se.SetICEAddressRewriteRules(webrtc.ICEAddressRewriteRule{
			External:        cfg.WebRTC.Candidates,
			AsCandidateType: webrtc.ICECandidateTypeHost,
		})
	}

	// Include loopback (127.0.0.1) as an ICE host candidate so same-host clients
	// connect via loopback UDP instead of the LAN interface, avoiding packet loss.
	se.SetIncludeLoopbackCandidate(true)

	me := &webrtc.MediaEngine{}
	if err := registerCodecs(me); err != nil {
		return err
	}

	// Register interceptors: NACK responder (retransmission), TWCC sender
	// (congestion control), and RTCP report generation.
	ir := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(me, ir); err != nil {
		return err
	}

	m.api = webrtc.NewAPI(
		webrtc.WithSettingEngine(se),
		webrtc.WithMediaEngine(me),
		webrtc.WithInterceptorRegistry(ir),
	)

	// Start HTTP signaling server.
	ln, err := s.MakeListener(cfg.WebRTC.Listen, cfg.WebRTC.TLS)
	if err != nil {
		return err
	}
	m.listener = ln

	mux := http.NewServeMux()
	mux.HandleFunc("POST /webrtc/whip/{path...}", m.handleWHIP)
	mux.HandleFunc("POST /webrtc/whep/{path...}", m.handleWHEP)
	mux.HandleFunc("DELETE /webrtc/session/{id}", m.handleDelete)
	mux.HandleFunc("PATCH /webrtc/session/{id}", m.handlePatch)
	mux.HandleFunc("OPTIONS /{path...}", m.handleOptions)

	var handler http.Handler = corsMiddleware(mux)
	if rl := cfg.Limits.RateLimit; rl.Enabled && rl.Rate > 0 {
		m.limiter = ratelimit.New(rl.Rate, rl.Burst)
		handler = m.limiter.Wrap(handler)
	}
	m.httpSrv = &http.Server{Handler: handler}

	proto := "http"
	if cfg.TLS.Configured() && (cfg.WebRTC.TLS == nil || *cfg.WebRTC.TLS) {
		proto = "https"
	}
	slog.Info("listening", "module", "webrtc", "proto", proto, "addr", ln.Addr())

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		if err := m.httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Error("serve error", "module", "webrtc", "error", err)
		}
	}()

	return nil
}

// Hooks returns the module's event hooks (none for WebRTC).
func (m *Module) Hooks() []core.HookRegistration { return nil }

// Close shuts down all sessions and the HTTP server.
func (m *Module) Close() error {
	// Close all active sessions.
	m.sessions.Range(func(key, value any) bool {
		if sess, ok := value.(*Session); ok {
			sess.Close()
		}
		return true
	})

	if m.httpSrv != nil {
		m.httpSrv.Close()
	}
	if m.limiter != nil {
		m.limiter.Close()
	}
	m.wg.Wait()
	slog.Info("stopped", "module", "webrtc")
	return nil
}

// Addr returns the listener address (useful for tests).
func (m *Module) Addr() net.Addr {
	if m.listener != nil {
		return m.listener.Addr()
	}
	return nil
}

// storeSession stores a session in the session map.
func (m *Module) storeSession(s *Session) {
	m.sessions.Store(s.id, s)
}

// removeSession removes a session from the session map.
func (m *Module) removeSession(s *Session) {
	m.sessions.Delete(s.id)
}

// findSession looks up a session by ID.
func (m *Module) findSession(id string) (*Session, bool) {
	v, ok := m.sessions.Load(id)
	if !ok {
		return nil, false
	}
	return v.(*Session), true
}

// iceServersFromConfig converts config ICE servers to pion ICE server structs.
func (m *Module) iceServersFromConfig() []webrtc.ICEServer {
	cfg := m.server.Config()
	var servers []webrtc.ICEServer
	for _, s := range cfg.WebRTC.ICEServers {
		servers = append(servers, webrtc.ICEServer{
			URLs:       s.URLs,
			Username:   s.Username,
			Credential: s.Credential,
		})
	}
	return servers
}

// handleDelete handles DELETE /webrtc/session/{id} to tear down a session.
func (m *Module) handleDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, ok := m.findSession(id)
	if !ok {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	sess.Close()
	w.WriteHeader(http.StatusOK)
}

// handlePatch handles PATCH /webrtc/session/{id} for ICE trickle candidates.
func (m *Module) handlePatch(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, ok := m.findSession(id)
	if !ok {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	contentType := r.Header.Get("Content-Type")
	if !strings.Contains(contentType, "application/trickle-ice-sdpfrag") {
		http.Error(w, "unsupported content type", http.StatusUnsupportedMediaType)
		return
	}

	// For now, ICE trickle is optional. Just acknowledge the request.
	_ = sess
	w.WriteHeader(http.StatusNoContent)
}

// handleOptions handles CORS preflight requests.
func (m *Module) handleOptions(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

// registerCodecs registers the codecs we support. Each codec gets exactly one
// entry (plus RTX) so the SDP answer stays compact and unambiguous.
func registerCodecs(me *webrtc.MediaEngine) error {
	// Audio: Opus.
	if err := me.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeOpus,
			ClockRate:   48000,
			Channels:    2,
			SDPFmtpLine: "minptime=10;useinbandfec=1",
		},
		PayloadType: 111,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return err
	}

	videoFeedback := []webrtc.RTCPFeedback{
		{Type: "goog-remb"},
		{Type: "ccm", Parameter: "fir"},
		{Type: "nack"},
		{Type: "nack", Parameter: "pli"},
	}

	// Video codecs: one entry per codec + RTX.
	videoCodecs := []struct {
		mime    string
		pt      webrtc.PayloadType
		rtxPT   webrtc.PayloadType
		fmtp    string
	}{
		{webrtc.MimeTypeVP8, 96, 97, ""},
		{webrtc.MimeTypeVP9, 98, 99, "profile-id=0"},
		{webrtc.MimeTypeH264, 106, 107, "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f"},
		{webrtc.MimeTypeH265, 116, 117, ""},
		{webrtc.MimeTypeAV1, 45, 46, ""},
	}

	for _, c := range videoCodecs {
		if err := me.RegisterCodec(webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType:     c.mime,
				ClockRate:    90000,
				SDPFmtpLine:  c.fmtp,
				RTCPFeedback: videoFeedback,
			},
			PayloadType: c.pt,
		}, webrtc.RTPCodecTypeVideo); err != nil {
			return err
		}

		if err := me.RegisterCodec(webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType:    webrtc.MimeTypeRTX,
				ClockRate:   90000,
				SDPFmtpLine: fmt.Sprintf("apt=%d", c.pt),
			},
			PayloadType: c.rtxPT,
		}, webrtc.RTPCodecTypeVideo); err != nil {
			return err
		}
	}

	return nil
}

// corsMiddleware adds CORS headers for browser-based WebRTC clients.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		w.Header().Set("Access-Control-Expose-Headers", "Location, Link")
		next.ServeHTTP(w, r)
	})
}
