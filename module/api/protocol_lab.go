package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
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
			views = append(views, h.sipLabView(session))
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
		writeJSON(w, http.StatusCreated, h.sipLabView(session))
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
			views = append(views, h.gbLabView(session))
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
		writeJSON(w, http.StatusCreated, h.gbLabView(session))
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

func (h *Handlers) sipLabView(session sipgateway.LabSessionSnapshot) protocolLabSessionView {
	return protocolLabSessionView{Session: session, Playback: h.labPlayback(session.StreamKey)}
}

func (h *Handlers) gbLabView(session gb28181.LabSessionSnapshot) protocolLabSessionView {
	return protocolLabSessionView{Session: session, Playback: h.labPlayback(session.StreamKey)}
}

func (h *Handlers) labPlayback(streamKey string) protocolLabPlayback {
	key := strings.TrimSpace(streamKey)
	playback := protocolLabPlayback{StreamKey: key}
	if key == "" {
		return playback
	}
	if stream, ok := h.server.StreamHub().Find(key); ok && stream.Publisher() != nil {
		playback.Available = true
	}
	if cfg := h.server.Config(); cfg.RTMP.Enabled && cfg.RTMP.Listen != "" {
		playback.RTMP = "rtmp://" + cfg.RTMP.Listen + "/" + key
	}
	if cfg := h.server.Config(); cfg.RTSP.Enabled && cfg.RTSP.Listen != "" {
		playback.RTSP = "rtsp://" + cfg.RTSP.Listen + "/" + key
	}
	if cfg := h.server.Config(); cfg.HTTP.Enabled {
		playback.HTTPFLV = "/" + key + ".flv"
		playback.WSFLV = "/ws/" + key + ".flv"
		playback.HTTPMPEGTS = "/" + key + ".ts"
		playback.FMP4 = "/" + key + ".mp4"
		playback.HLS = "/" + key + ".m3u8"
		playback.DASH = "/" + key + ".mpd"
	}
	if cfg := h.server.Config(); cfg.WebRTC.Enabled {
		playback.WHEP = "/webrtc/whep/" + key + "?mode=realtime"
		playback.WHEPLive = "/webrtc/whep/" + key + "?mode=live"
	}
	return playback
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
