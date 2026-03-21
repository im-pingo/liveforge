package api

import (
	"net/http"

	"github.com/im-pingo/liveforge/core"
)

// RegisterRoutes registers all API and console routes on the given mux.
// This allows any HTTP server (httpstream, standalone API, etc.) to serve the management API.
func RegisterRoutes(mux *http.ServeMux, s *core.Server) {
	h := NewHandlers(s)
	mux.HandleFunc("GET /api/v1/streams", h.handleStreams)
	mux.HandleFunc("GET /api/v1/server/info", h.handleServerInfo)
	mux.HandleFunc("GET /api/v1/server/stats", h.handleServerStats)
	mux.HandleFunc("GET /api/v1/server/health", h.handleHealth)
	mux.HandleFunc("DELETE /api/v1/streams/", h.handleStreamDelete)
	mux.HandleFunc("POST /api/v1/streams/", h.handleKick)
	mux.HandleFunc("GET /api/v1/streams/", h.handleStreamDetail)
	mux.HandleFunc("GET /console", h.handleConsole)
}
