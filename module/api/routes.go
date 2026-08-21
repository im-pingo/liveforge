package api

import (
	"net/http"
	"net/http/pprof"

	"github.com/im-pingo/liveforge/core"
)

// RegisterRoutes registers all API and console routes on the given mux.
// This allows any HTTP server (httpstream, standalone API, etc.) to serve the management API.
func RegisterRoutes(mux *http.ServeMux, s *core.Server) {
	registerRoutes(mux, s, mustNewSessionManager())
}

func registerRoutes(mux *http.ServeMux, s *core.Server, sessions *sessionManager) {
	h := newHandlersWithSessions(s, sessions)
	mux.HandleFunc("GET /api/v1/streams", h.handleStreams)
	mux.HandleFunc("GET /api/v1/server/info", h.handleServerInfo)
	mux.HandleFunc("GET /api/v1/server/stats", h.handleServerStats)
	mux.HandleFunc("GET /api/v1/server/health", h.handleHealth)
	mux.HandleFunc("GET /api/v1/dvr/status", h.handleDVRStatus)
	mux.HandleFunc("GET /api/v1/config", h.handleConfigGet)
	mux.HandleFunc("PATCH /api/v1/config", h.handleConfigPatch)
	mux.HandleFunc("POST /api/v1/config/password", h.handlePasswordChange)
	mux.HandleFunc("DELETE /api/v1/streams/", h.handleStreamDelete)
	mux.HandleFunc("POST /api/v1/streams/", h.handleKick)
	mux.HandleFunc("GET /api/v1/streams/", h.handleStreamDetail)
	mux.HandleFunc("GET /console", h.handleConsole)
	mux.HandleFunc("GET /console/cert.pem", h.handleCertDownload)
	mux.HandleFunc("GET /debug/webrtc", h.handleDebugWebRTC)
	mux.Handle("GET /console/static/", staticHandler())

	// pprof endpoints (protected by API auth middleware)
	mux.HandleFunc("GET /debug/pprof/", pprof.Index)
	mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)

	// Register cross-module API handlers (e.g., WebSocket notifications).
	for pattern, handler := range s.APIHandlers() {
		mux.Handle(pattern, handler)
	}
}
