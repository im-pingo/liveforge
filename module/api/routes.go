package api

import (
	"net/http"
	"net/http/pprof"

	"github.com/im-pingo/liveforge/core"
)

// RegisterRoutes registers all API and console routes on the given mux.
// This allows any HTTP server (httpstream, standalone API, etc.) to serve the management API.
func RegisterRoutes(mux *http.ServeMux, s *core.Server) {
	sessions := mustNewSessionManager()
	routes := http.NewServeMux()
	patterns := registerRoutes(routes, s, sessions)
	secured := buildDynamicAuthHandler(routes, s, sessions)
	for _, pattern := range patterns {
		mux.Handle(pattern, secured)
	}
}

func securedRoutes(s *core.Server, sessions *sessionManager) http.Handler {
	routes := http.NewServeMux()
	registerRoutes(routes, s, sessions)
	return buildDynamicAuthHandler(routes, s, sessions)
}

type routeRegistrar struct {
	mux      *http.ServeMux
	patterns []string
}

func (r *routeRegistrar) HandleFunc(pattern string, handler http.HandlerFunc) {
	r.patterns = append(r.patterns, pattern)
	r.mux.HandleFunc(pattern, handler)
}

func (r *routeRegistrar) Handle(pattern string, handler http.Handler) {
	r.patterns = append(r.patterns, pattern)
	r.mux.Handle(pattern, handler)
}

func registerRoutes(mux *http.ServeMux, s *core.Server, sessions *sessionManager) []string {
	routes := &routeRegistrar{mux: mux}
	h := newHandlersWithSessions(s, sessions)
	routes.HandleFunc("GET /api/v1/streams", h.handleStreams)
	routes.HandleFunc("GET /api/v1/server/info", h.handleServerInfo)
	routes.HandleFunc("GET /api/v1/server/stats", h.handleServerStats)
	routes.HandleFunc("GET /api/v1/server/health", h.handleHealth)
	routes.HandleFunc("GET /api/v1/dvr/status", h.handleDVRStatus)
	routes.HandleFunc("GET /api/v1/config", h.handleConfigGet)
	routes.HandleFunc("PATCH /api/v1/config", h.handleConfigPatch)
	routes.HandleFunc("POST /api/v1/config/password", h.handlePasswordChange)
	routes.HandleFunc("DELETE /api/v1/streams/", h.handleStreamDelete)
	routes.HandleFunc("POST /api/v1/streams/", h.handleKick)
	routes.HandleFunc("GET /api/v1/streams/", h.handleStreamDetail)
	routes.HandleFunc("GET /console", h.handleConsole)
	routes.HandleFunc("GET /console/cert.pem", h.handleCertDownload)
	routes.HandleFunc("GET /debug/webrtc", h.handleDebugWebRTC)
	routes.Handle("GET /console/static/", staticHandler())

	// pprof endpoints (protected by API auth middleware)
	routes.HandleFunc("GET /debug/pprof/", pprof.Index)
	routes.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
	routes.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
	routes.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
	routes.HandleFunc("GET /debug/pprof/trace", pprof.Trace)

	// Register cross-module API handlers (e.g., WebSocket notifications).
	for pattern, handler := range s.APIHandlers() {
		routes.Handle(pattern, handler)
	}

	// Login is served directly by the auth middleware rather than the raw mux.
	routes.patterns = append(routes.patterns, "/console/login")
	return routes.patterns
}
