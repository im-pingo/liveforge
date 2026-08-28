package api

import (
	"net/http"
	"net/http/pprof"

	"github.com/im-pingo/liveforge/core"
)

// RegisterRoutes registers all API and console routes on the given mux.
// This allows any HTTP server (httpstream, standalone API, etc.) to serve the management API.
func RegisterRoutes(mux *http.ServeMux, s *core.Server) {
	registerRoutes(mux, s, nil)
}

func registerRoutes(mux *http.ServeMux, s *core.Server, audit *AuditStore) {
	h := newHandlersWithAudit(s, audit)
	mux.HandleFunc("GET /api/v1/streams", h.handleStreams)
	mux.HandleFunc("GET /api/v1/server/info", h.handleServerInfo)
	mux.HandleFunc("GET /api/v1/server/stats", h.handleServerStats)
	mux.HandleFunc("GET /api/v1/server/config", h.handleConfigStatus)
	mux.HandleFunc("GET /api/v1/server/config/document", h.handleConfigDocument)
	mux.HandleFunc("GET /api/v1/server/config/schema", h.handleConfigSchema)
	mux.HandleFunc("POST /api/v1/server/config/validate", h.handleConfigValidate)
	mux.HandleFunc("POST /api/v1/server/config/apply", h.handleConfigApply)
	mux.HandleFunc("POST /api/v1/server/config/refresh", h.handleConfigRefresh)
	mux.HandleFunc("GET /api/v1/server/health", h.handleHealth)
	mux.HandleFunc("GET /api/v1/audit", h.handleAudit)
	mux.HandleFunc("GET /api/v1/security/status", h.handleSecurityStatus)
	mux.HandleFunc("GET /api/v1/sipgateway/calls", h.handleSIPGatewayCalls)
	mux.HandleFunc("GET /api/v1/sipgateway/test", h.handleSIPGatewaySelfTest)
	mux.HandleFunc("GET /api/v1/sipgateway/lab/sessions", h.handleSIPGatewayLabSessions)
	mux.HandleFunc("POST /api/v1/sipgateway/lab/sessions", h.handleSIPGatewayLabSessions)
	mux.HandleFunc("DELETE /api/v1/sipgateway/lab/sessions/{id}", h.handleSIPGatewayLabSession)
	mux.HandleFunc("POST /api/v1/sipgateway/calls", h.handleSIPGatewayDial)
	mux.HandleFunc("GET /api/v1/sipgateway/calls/{call_id}", h.handleSIPGatewayCall)
	mux.HandleFunc("DELETE /api/v1/sipgateway/calls/{call_id}", h.handleSIPGatewayCall)
	mux.HandleFunc("GET /api/v1/cluster/status", h.handleClusterStatus)
	mux.HandleFunc("GET /api/v1/recordings", h.handleRecordings)
	mux.HandleFunc("GET /api/v1/recordings/status", h.handleRecordingStatus)
	mux.HandleFunc("GET /api/v1/recordings/{recording_path...}", h.handleRecordingRoute)
	mux.HandleFunc("DELETE /api/v1/recordings/{recording_path...}", h.handleRecordingRoute)
	mux.HandleFunc("GET /api/v1/dvr/status", h.handleDVRStatus)
	mux.HandleFunc("GET /api/v1/dvr/sessions/{stream_key...}", h.handleDVRSession)
	mux.HandleFunc("GET /api/v1/gb28181/lab/sessions", h.handleGB28181LabSessions)
	mux.HandleFunc("POST /api/v1/gb28181/lab/sessions", h.handleGB28181LabSessions)
	mux.HandleFunc("DELETE /api/v1/gb28181/lab/sessions/{id}", h.handleGB28181LabSession)
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
