package api

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed console.html
var consoleHTML []byte

//go:embed debug_webrtc.html
var debugWebRTCHTML []byte

//go:embed static
var staticFS embed.FS

// staticHandler returns an http.Handler that serves embedded static files
// under the /console/static/ URL prefix.
func staticHandler() http.Handler {
	sub, _ := fs.Sub(staticFS, "static")
	return http.StripPrefix("/console/static/", http.FileServer(http.FS(sub)))
}

func (h *Handlers) handleConsole(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(consoleHTML)
}

func (h *Handlers) handleDebugWebRTC(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(debugWebRTCHTML)
}

// handleCertDownload serves the auto-generated self-signed certificate as a
// downloadable PEM file. Users can install this into their OS/browser trust
// store to avoid certificate warnings.
func (h *Handlers) handleCertDownload(w http.ResponseWriter, r *http.Request) {
	pemData := h.server.AutoCertPEM()
	if pemData == nil {
		http.Error(w, "no auto-generated certificate available", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.Header().Set("Content-Disposition", "attachment; filename=\"liveforge-cert.pem\"")
	w.Write(pemData)
}
