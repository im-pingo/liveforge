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
}

// NewHandlers creates API handlers backed by the given server.
func NewHandlers(s *core.Server) *Handlers {
	return &Handlers{server: s}
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
	Key         string              `json:"key"`
	State       string              `json:"state"`
	Publisher   string              `json:"publisher"`
	VideoCodec  string              `json:"video_codec"`
	AudioCodec  string              `json:"audio_codec"`
	GOPCacheLen int                 `json:"gop_cache_len"`
	Subscribers map[string]int      `json:"subscribers"`
	Stats       *StreamStatsDetail  `json:"stats,omitempty"`
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
	resp := buildStreamsResponse(h.server.StreamHub(), true)
	writeJSON(w, http.StatusOK, resp)
}

func buildStreamInfo(stream *core.Stream, includeStats bool) StreamInfo {
	state := stream.State()

	// Merge muxer-level (flv/ts/mp4) and protocol-level (rtmp) subscriber counts.
	subs := stream.MuxerManager().Formats()
	for proto, count := range stream.Subscribers() {
		subs[proto] = count
	}

	info := StreamInfo{
		Key:         stream.Key(),
		State:       state.String(),
		GOPCacheLen: stream.GOPCacheLen(),
		Subscribers: subs,
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

func buildStreamsResponse(hub *core.StreamHub, includeStats bool) StreamsResponse {
	keys := hub.Keys()
	streams := make([]StreamInfo, 0, len(keys))

	for _, key := range keys {
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
	stream.RemovePublisher()

	h.server.GetEventBus().Emit(core.EventPublishStop, &core.EventContext{
		StreamKey: key,
		Protocol:  "api-kick",
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

func (h *Handlers) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "healthy"})
}
