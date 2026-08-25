package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/im-pingo/liveforge/core"
)

// Handlers holds the API handler methods. It only depends on *core.Server,
// so it can be registered on any http.ServeMux (httpstream, standalone API, etc.).
type Handlers struct {
	server *core.Server
	audit  *AuditStore
}

// NewHandlers creates API handlers backed by the given server.
func NewHandlers(s *core.Server) *Handlers {
	return &Handlers{server: s, audit: NewAuditStore(s.Config().API.Audit.MaxEntries)}
}

func newHandlersWithAudit(s *core.Server, audit *AuditStore) *Handlers {
	if audit == nil {
		audit = NewAuditStore(s.Config().API.Audit.MaxEntries)
	}
	return &Handlers{server: s, audit: audit}
}

// apiResponse is the standard API response envelope.
type apiResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func writeJSON(w http.ResponseWriter, code int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(apiResponse{Code: 0, Message: "ok", Data: data})
}

func writeError(w http.ResponseWriter, httpCode int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpCode)
	json.NewEncoder(w).Encode(apiResponse{Code: httpCode, Message: msg})
}

// StreamInfo represents a single stream in the API response.
type StreamInfo struct {
	Key            string             `json:"key"`
	State          string             `json:"state"`
	Publisher      string             `json:"publisher"`
	VideoCodec     string             `json:"video_codec"`
	AudioCodec     string             `json:"audio_codec"`
	GOPCacheLen    int                `json:"gop_cache_len"`
	GOPVideoFrames int                `json:"gop_video_frames"`
	GOPAudioFrames int                `json:"gop_audio_frames"`
	GOPDurationMs  int64              `json:"gop_duration_ms"`
	Subscribers    map[string]int     `json:"subscribers"`
	Stats          *StreamStatsDetail `json:"stats,omitempty"`
}

// StreamStatsDetail contains detailed stream statistics.
type StreamStatsDetail struct {
	BytesIn     int64   `json:"bytes_in"`
	VideoFrames int64   `json:"video_frames"`
	AudioFrames int64   `json:"audio_frames"`
	UptimeSec   int64   `json:"uptime_sec"`
	BitrateKbps int64   `json:"bitrate_kbps"`
	FPS         float64 `json:"fps"`
}

// StreamsResponse is the top-level JSON response for GET /api/v1/streams.
type StreamsResponse struct {
	Streams []StreamInfo `json:"streams"`
}

func (h *Handlers) handleStreams(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	resp := buildStreamsResponse(h.server.StreamHub(), true, query)
	writeJSON(w, http.StatusOK, resp)
}

func buildStreamInfo(stream *core.Stream, includeStats bool) StreamInfo {
	state := stream.State()

	// Merge muxer-level (flv/ts/mp4) and protocol-level (rtmp) subscriber counts.
	subs := stream.MuxerManager().Formats()
	for proto, count := range stream.Subscribers() {
		subs[proto] = count
	}

	gopDetail := stream.GOPCacheDetail()

	info := StreamInfo{
		Key:            stream.Key(),
		State:          state.String(),
		GOPCacheLen:    gopDetail.TotalFrames,
		GOPVideoFrames: gopDetail.VideoFrames,
		GOPAudioFrames: gopDetail.AudioFrames,
		GOPDurationMs:  gopDetail.DurationMs,
		Subscribers:    subs,
	}

	if pub := stream.Publisher(); pub != nil {
		info.Publisher = pub.ID()
		if mi := pub.MediaInfo(); mi != nil {
			info.VideoCodec = mi.VideoCodec.String()
			info.AudioCodec = mi.AudioCodec.String()
		}
	}

	if includeStats {
		stats := stream.Stats()
		info.Stats = &StreamStatsDetail{
			BytesIn:     stats.BytesIn,
			VideoFrames: stats.VideoFrames,
			AudioFrames: stats.AudioFrames,
			UptimeSec:   int64(stats.Uptime.Seconds()),
			BitrateKbps: stats.BitrateKbps,
			FPS:         stats.FPS,
		}
	}

	return info
}

func buildStreamsResponse(hub *core.StreamHub, includeStats bool, filter string) StreamsResponse {
	keys := hub.Keys()
	streams := make([]StreamInfo, 0, len(keys))

	for _, key := range keys {
		if filter != "" && !strings.Contains(key, filter) {
			continue
		}
		stream, ok := hub.Find(key)
		if !ok {
			continue
		}
		if stream.State() == core.StreamStateDestroying {
			continue
		}
		streams = append(streams, buildStreamInfo(stream, includeStats))
	}

	return StreamsResponse{Streams: streams}
}

// extractStreamKey extracts the stream key from path after the given prefix.
// e.g., prefix="/api/v1/streams/", path="/api/v1/streams/live/test" → "live/test"
func extractStreamKey(path, prefix string) string {
	return strings.TrimPrefix(path, prefix)
}

func (h *Handlers) handleStreamDetail(w http.ResponseWriter, r *http.Request) {
	key := extractStreamKey(r.URL.Path, "/api/v1/streams/")
	if key == "" {
		writeError(w, http.StatusBadRequest, "missing stream key")
		return
	}

	// Strip /kick suffix if present (shouldn't reach here, but be safe)
	key = strings.TrimSuffix(key, "/kick")

	stream, ok := h.server.StreamHub().Find(key)
	if !ok || stream.State() == core.StreamStateDestroying {
		writeError(w, http.StatusNotFound, "stream not found")
		return
	}

	writeJSON(w, http.StatusOK, buildStreamInfo(stream, true))
}

func (h *Handlers) handleStreamDelete(w http.ResponseWriter, r *http.Request) {
	key := extractStreamKey(r.URL.Path, "/api/v1/streams/")
	if key == "" {
		writeError(w, http.StatusBadRequest, "missing stream key")
		return
	}

	stream, ok := h.server.StreamHub().Find(key)
	if !ok {
		writeError(w, http.StatusNotFound, "stream not found")
		return
	}

	stream.Close()
	h.server.StreamHub().Remove(key)
	writeJSON(w, http.StatusOK, nil)
}

func (h *Handlers) handleKick(w http.ResponseWriter, r *http.Request) {
	// Path: /api/v1/streams/{key}/kick — extract key by removing prefix and /kick suffix
	key := extractStreamKey(r.URL.Path, "/api/v1/streams/")
	if !strings.HasSuffix(key, "/kick") {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	key = strings.TrimSuffix(key, "/kick")
	if key == "" {
		writeError(w, http.StatusBadRequest, "missing stream key")
		return
	}

	stream, ok := h.server.StreamHub().Find(key)
	if !ok || stream.State() == core.StreamStateDestroying {
		writeError(w, http.StatusNotFound, "stream not found")
		return
	}

	pub := stream.Publisher()
	if pub == nil {
		writeError(w, http.StatusConflict, "stream has no publisher")
		return
	}

	pub.Close()
	stream.RemovePublisherIf(pub)

	h.server.GetEventBus().Emit(core.EventPublishStop, &core.EventContext{
		StreamKey:   key,
		PublisherID: pub.ID(),
		Protocol:    "api-kick",
	}) //nolint:errcheck

	writeJSON(w, http.StatusOK, nil)
}

// ServerInfo is the response for GET /api/v1/server/info.
type ServerInfo struct {
	Version   string            `json:"version"`
	Uptime    int64             `json:"uptime_sec"`
	Modules   []string          `json:"modules"`
	Endpoints map[string]string `json:"endpoints,omitempty"`
}

func (h *Handlers) handleServerInfo(w http.ResponseWriter, r *http.Request) {
	cfg := h.server.Config()
	endpoints := make(map[string]string)
	if cfg.HTTP.Enabled {
		endpoints["http"] = cfg.HTTP.Listen
	}
	if cfg.WebRTC.Enabled {
		endpoints["webrtc"] = cfg.WebRTC.Listen
	}
	if cfg.RTMP.Enabled {
		endpoints["rtmp"] = cfg.RTMP.Listen
	}
	if cfg.RTSP.Enabled {
		endpoints["rtsp"] = cfg.RTSP.Listen
	}
	if cfg.DVR.Enabled {
		endpoints["dvr"] = cfg.DVR.Listen
	}
	writeJSON(w, http.StatusOK, ServerInfo{
		Version:   core.Version,
		Uptime:    int64(time.Since(h.server.StartTime()).Seconds()),
		Modules:   h.server.ModuleNames(),
		Endpoints: endpoints,
	})
}

// ServerStats is the response for GET /api/v1/server/stats.
type ServerStats struct {
	Streams     int   `json:"streams"`
	Connections int64 `json:"connections"`
}

func (h *Handlers) handleServerStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, ServerStats{
		Streams:     h.server.StreamHub().Count(),
		Connections: h.server.ConnectionCount(),
	})
}

// ConfigRuntimeStatus is the redacted runtime configuration loader status.
type ConfigRuntimeStatus struct {
	Enabled                        bool      `json:"enabled"`
	Source                         string    `json:"source,omitempty"`
	ActiveVersion                  string    `json:"active_version,omitempty"`
	ActiveHash                     string    `json:"active_hash,omitempty"`
	LastAttempt                    time.Time `json:"last_attempt,omitempty"`
	LastSuccess                    time.Time `json:"last_success,omitempty"`
	ConsecutiveFailures            uint64    `json:"consecutive_failures"`
	LastError                      string    `json:"last_error,omitempty"`
	PendingRestart                 []string  `json:"pending_restart,omitempty"`
	CallbackFailures               uint64    `json:"callback_failures"`
	DroppedCallbacks               uint64    `json:"dropped_callbacks"`
	ConfigChangesAccepted          uint64    `json:"config_changes_accepted"`
	ConfigChangesRejected          uint64    `json:"config_changes_rejected"`
	ConfigChangesApplicationFailed uint64    `json:"config_changes_application_failed"`
}

func (h *Handlers) handleConfigStatus(w http.ResponseWriter, r *http.Request) {
	manager := h.server.ConfigManager()
	if manager == nil {
		writeJSON(w, http.StatusOK, ConfigRuntimeStatus{})
		return
	}
	status := manager.Status()
	writeJSON(w, http.StatusOK, ConfigRuntimeStatus{
		Enabled:                        true,
		Source:                         status.Source,
		ActiveVersion:                  status.ActiveVersion.Value,
		ActiveHash:                     status.ActiveVersion.Hash,
		LastAttempt:                    status.LastAttempt,
		LastSuccess:                    status.LastSuccess,
		ConsecutiveFailures:            status.ConsecutiveFailures,
		LastError:                      status.LastError,
		PendingRestart:                 status.PendingRestart,
		CallbackFailures:               status.CallbackFailures,
		DroppedCallbacks:               status.DroppedCallbacks,
		ConfigChangesAccepted:          status.ConfigChangesAccepted,
		ConfigChangesRejected:          status.ConfigChangesRejected,
		ConfigChangesApplicationFailed: status.ConfigChangesApplicationFailed,
	})
}

func (h *Handlers) handleConfigRefresh(w http.ResponseWriter, r *http.Request) {
	manager := h.server.ConfigManager()
	if manager == nil {
		writeError(w, http.StatusServiceUnavailable, "runtime config manager unavailable")
		return
	}
	if err := manager.Refresh(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "scheduled"})
}

func (h *Handlers) handleAudit(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.audit.Entries())
}

type SecurityTokenStatus struct {
	Name string `json:"name"`
	Role string `json:"role"`
}

type SecurityStatus struct {
	LegacyBearerConfigured bool                  `json:"legacy_bearer_configured"`
	Tokens                 []SecurityTokenStatus `json:"tokens"`
	ConsoleConfigured      bool                  `json:"console_configured"`
	ConsoleRole            string                `json:"console_role,omitempty"`
	AuditEnabled           bool                  `json:"audit_enabled"`
	AuditEntries           int                   `json:"audit_entries"`
	AuditEvents            uint64                `json:"audit_events_total"`
}

func (h *Handlers) handleSecurityStatus(w http.ResponseWriter, r *http.Request) {
	cfg := h.server.Config().API
	tokens := make([]SecurityTokenStatus, 0, len(cfg.Auth.Tokens))
	for _, binding := range cfg.Auth.Tokens {
		tokens = append(tokens, SecurityTokenStatus{Name: binding.Name, Role: defaultRole(binding.Role)})
	}
	entries := h.audit.Entries()
	writeJSON(w, http.StatusOK, SecurityStatus{
		LegacyBearerConfigured: cfg.Auth.BearerToken != "",
		Tokens:                 tokens,
		ConsoleConfigured:      cfg.Console.Username != "",
		ConsoleRole:            defaultRole(cfg.Console.Role),
		AuditEnabled:           true,
		AuditEntries:           len(entries),
		AuditEvents:            h.audit.Total(),
	})
}

func (h *Handlers) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "healthy"})
}
