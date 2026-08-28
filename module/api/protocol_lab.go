package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/im-pingo/liveforge/module/gb28181"
	"github.com/im-pingo/liveforge/module/sipgateway"
)

type gb28181LabProvider interface {
	StartLabSession(context.Context, gb28181.LabSessionRequest) (gb28181.LabSessionSnapshot, error)
	ListLabSessions() []gb28181.LabSessionSnapshot
	StopLabSession(string) error
}

type protocolLabPlayback struct {
	StreamKey  string `json:"stream_key"`
	Available  bool   `json:"available"`
	RTMP       string `json:"rtmp,omitempty"`
	RTSP       string `json:"rtsp,omitempty"`
	HTTPFLV    string `json:"http_flv,omitempty"`
	WSFLV      string `json:"ws_flv,omitempty"`
	HTTPMPEGTS string `json:"http_ts,omitempty"`
	FMP4       string `json:"fmp4,omitempty"`
	HLS        string `json:"hls,omitempty"`
	DASH       string `json:"dash,omitempty"`
	WHEP       string `json:"whep,omitempty"`
	WHEPLive   string `json:"whep_live,omitempty"`
}

type protocolLabSessionView struct {
	Session  any                 `json:"session"`
	Playback protocolLabPlayback `json:"playback"`
}

type protocolLabListResponse struct {
	Sessions []protocolLabSessionView `json:"sessions"`
}

func (h *Handlers) handleSIPGatewayLabSessions(w http.ResponseWriter, r *http.Request) {
	provider, ok := h.sipGatewayProvider()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "SIP gateway lab unavailable")
		return
	}
	switch r.Method {
	case http.MethodGet:
		views := make([]protocolLabSessionView, 0)
		for _, session := range provider.ListLabSessions() {
			views = append(views, h.sipLabView(r, session))
		}
		writeJSON(w, http.StatusOK, protocolLabListResponse{Sessions: views})
	case http.MethodPost:
		var request sipgateway.LabSessionRequest
		if err := decodeStrictJSON(r, &request); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		session, err := provider.StartLabSession(r.Context(), request)
		if err != nil {
			writeSIPLabError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, h.sipLabView(r, session))
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (h *Handlers) handleSIPGatewayLabSession(w http.ResponseWriter, r *http.Request) {
	provider, ok := h.sipGatewayProvider()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "SIP gateway lab unavailable")
		return
	}
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if id := strings.TrimSpace(r.PathValue("id")); id != "" {
		if err := provider.StopLabSession(id); err != nil {
			writeSIPLabError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "stopped", "id": id})
		return
	}
	writeError(w, http.StatusBadRequest, "missing lab session ID")
}

func (h *Handlers) handleGB28181LabSessions(w http.ResponseWriter, r *http.Request) {
	provider, ok := h.gb28181LabProvider()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "GB28181 lab unavailable")
		return
	}
	switch r.Method {
	case http.MethodGet:
		views := make([]protocolLabSessionView, 0)
		for _, session := range provider.ListLabSessions() {
			views = append(views, h.gbLabView(r, session))
		}
		writeJSON(w, http.StatusOK, protocolLabListResponse{Sessions: views})
	case http.MethodPost:
		var request gb28181.LabSessionRequest
		if err := decodeStrictJSON(r, &request); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		session, err := provider.StartLabSession(r.Context(), request)
		if err != nil {
			writeGBLabError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, h.gbLabView(r, session))
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (h *Handlers) handleGB28181LabSession(w http.ResponseWriter, r *http.Request) {
	provider, ok := h.gb28181LabProvider()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "GB28181 lab unavailable")
		return
	}
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if id := strings.TrimSpace(r.PathValue("id")); id != "" {
		if err := provider.StopLabSession(id); err != nil {
			writeGBLabError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "stopped", "id": id})
		return
	}
	writeError(w, http.StatusBadRequest, "missing lab session ID")
}

func (h *Handlers) gb28181LabProvider() (gb28181LabProvider, bool) {
	module := h.server.ModuleByName("gb28181")
	provider, ok := module.(gb28181LabProvider)
	return provider, ok
}

func (h *Handlers) sipLabView(r *http.Request, session sipgateway.LabSessionSnapshot) protocolLabSessionView {
	return protocolLabSessionView{Session: session, Playback: h.labPlayback(r, session.StreamKey)}
}

func (h *Handlers) gbLabView(r *http.Request, session gb28181.LabSessionSnapshot) protocolLabSessionView {
	return protocolLabSessionView{Session: session, Playback: h.labPlayback(r, session.StreamKey)}
}

func (h *Handlers) labPlayback(r *http.Request, streamKey string) protocolLabPlayback {
	key := strings.TrimSpace(streamKey)
	playback := protocolLabPlayback{StreamKey: key}
	if key == "" {
		return playback
	}
	if stream, ok := h.server.StreamHub().Find(key); ok && stream.Publisher() != nil {
		playback.Available = true
	}
	escapedKey := escapeStreamPath(key)
	if cfg := h.server.Config(); cfg.RTMP.Enabled && cfg.RTMP.Listen != "" {
		endpoint := endpointAddress(h.server, "rtmp", cfg.RTMP.Listen)
		playback.RTMP = "rtmp://" + playbackEndpointAuthority(r, endpoint) + "/" + escapedKey
	}
	if cfg := h.server.Config(); cfg.RTSP.Enabled && cfg.RTSP.Listen != "" {
		endpoint := endpointAddress(h.server, "rtsp", cfg.RTSP.Listen)
		playback.RTSP = "rtsp://" + playbackEndpointAuthority(r, endpoint) + "/" + escapedKey
	}
	if cfg := h.server.Config(); cfg.HTTP.Enabled {
		playback.HTTPFLV = "/" + escapedKey + ".flv"
		playback.WSFLV = "/ws/" + escapedKey + ".flv"
		playback.HTTPMPEGTS = "/" + escapedKey + ".ts"
		playback.FMP4 = "/" + escapedKey + ".mp4"
		playback.HLS = "/" + escapedKey + ".m3u8"
		playback.DASH = "/" + escapedKey + ".mpd"
	}
	if cfg := h.server.Config(); cfg.WebRTC.Enabled {
		playback.WHEP = "/webrtc/whep/" + escapedKey + "?mode=realtime"
		playback.WHEPLive = "/webrtc/whep/" + escapedKey + "?mode=live"
	}
	return playback
}

func escapeStreamPath(key string) string {
	segments := strings.Split(key, "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	return strings.Join(segments, "/")
}

func playbackEndpointAuthority(r *http.Request, endpoint string) string {
	value := strings.TrimSpace(endpoint)
	fallbackHost := "127.0.0.1"
	if r != nil {
		fallbackHost = requestHostname(r.Host, fallbackHost)
	}
	if strings.HasPrefix(value, ":") {
		return net.JoinHostPort(fallbackHost, strings.TrimPrefix(value, ":"))
	}
	host, port, err := net.SplitHostPort(value)
	if err != nil {
		return value
	}
	if isWildcardEndpointHost(host) {
		return net.JoinHostPort(fallbackHost, port)
	}
	return value
}

func requestHostname(authority, fallback string) string {
	if host, _, err := net.SplitHostPort(authority); err == nil && host != "" {
		return strings.Trim(host, "[]")
	}
	if host := strings.Trim(authority, "[]"); host != "" {
		return host
	}
	return fallback
}

func isWildcardEndpointHost(host string) bool {
	host = strings.Trim(host, "[]")
	return host == "" || host == "*" || host == "0.0.0.0" || host == "::" || host == "0:0:0:0:0:0:0:0"
}

func decodeStrictJSON(r *http.Request, destination any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("request body contains multiple JSON values")
	}
	return nil
}

func writeSIPLabError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, sipgateway.ErrLabInvalidRequest):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, sipgateway.ErrLabDuplicateIdentity):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, sipgateway.ErrLabSessionNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, sipgateway.ErrGatewayDisabled), errors.Is(err, sipgateway.ErrGatewayClosed), errors.Is(err, sipgateway.ErrLabManagerUnimplemented):
		writeError(w, http.StatusServiceUnavailable, err.Error())
	default:
		writeError(w, http.StatusBadGateway, "SIP lab setup failed")
	}
}

func writeGBLabError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, gb28181.ErrLabInvalidRequest):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, gb28181.ErrLabDuplicateIdentity):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, gb28181.ErrLabSessionNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, gb28181.ErrLabManagerUnimplemented):
		writeError(w, http.StatusServiceUnavailable, err.Error())
	default:
		writeError(w, http.StatusBadGateway, "GB28181 lab setup failed")
	}
}
