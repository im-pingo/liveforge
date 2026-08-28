package api

import (
	"errors"
	"mime"
	"net/http"
	"path"
	"strings"

	"github.com/im-pingo/liveforge/module/dvr"
	"github.com/im-pingo/liveforge/module/record"
)

func (h *Handlers) recordingProvider() (record.RecordingProvider, bool) {
	module := h.server.ModuleByName("record")
	provider, ok := module.(record.RecordingProvider)
	return provider, ok
}

func (h *Handlers) handleRecordings(w http.ResponseWriter, r *http.Request) {
	provider, ok := h.recordingProvider()
	if !ok {
		writeJSON(w, http.StatusOK, []record.RecordingInfo{})
		return
	}
	items, err := provider.ListRecordings(r.Context())
	if err != nil {
		writeRecordingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (h *Handlers) handleRecordingStatus(w http.ResponseWriter, r *http.Request) {
	provider, ok := h.recordingProvider()
	if !ok {
		writeJSON(w, http.StatusOK, record.RecordingStatusSnapshot{
			Enabled:   false,
			Available: true,
			State:     record.RecordingDisabled,
			Reason:    "recording disabled",
			Sessions:  []record.RecordingSessionStatus{},
		})
		return
	}
	writeJSON(w, http.StatusOK, provider.RecordingStatus(r.Context()))
}

func (h *Handlers) handleRecordingRoute(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("recording_path")
	r.SetPathValue("id", id)
	if r.Method == http.MethodDelete {
		h.handleRecording(w, r)
		return
	}
	switch strings.ToLower(strings.TrimSpace(r.URL.Query().Get("action"))) {
	case "download":
		h.handleRecordingDownload(w, r)
		return
	case "play":
		h.handleRecordingPlay(w, r)
		return
	}
	provider, ok := h.recordingProvider()
	if !ok {
		h.handleRecording(w, r)
		return
	}
	info, err := provider.Recording(r.Context(), id)
	if err == nil {
		writeJSON(w, http.StatusOK, info)
		return
	}
	if !errors.Is(err, record.ErrRecordingNotFound) {
		writeRecordingError(w, err)
		return
	}
	if strings.HasSuffix(id, "/download") {
		r.SetPathValue("id", strings.TrimSuffix(id, "/download"))
		h.handleRecordingDownload(w, r)
		return
	}
	if strings.HasSuffix(id, "/play") {
		r.SetPathValue("id", strings.TrimSuffix(id, "/play"))
		h.handleRecordingPlay(w, r)
		return
	}
	writeRecordingError(w, err)
}

func (h *Handlers) handleRecording(w http.ResponseWriter, r *http.Request) {
	provider, ok := h.recordingProvider()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "recording module unavailable")
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing recording id")
		return
	}
	if r.Method == http.MethodDelete {
		if err := provider.DeleteRecording(r.Context(), id); err != nil {
			writeRecordingError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, nil)
		return
	}
	info, err := provider.Recording(r.Context(), id)
	if err != nil {
		writeRecordingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (h *Handlers) handleRecordingDownload(w http.ResponseWriter, r *http.Request) {
	provider, ok := h.recordingProvider()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "recording module unavailable")
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing recording id")
		return
	}
	reader, info, err := provider.OpenRecording(r.Context(), id)
	if err != nil {
		writeRecordingError(w, err)
		return
	}
	defer reader.Close()
	if disposition := mime.FormatMediaType("attachment", map[string]string{"filename": path.Base(info.ID)}); disposition != "" {
		w.Header().Set("Content-Disposition", disposition)
	}
	modified := info.CompletedAt
	if modified.IsZero() {
		modified = info.StartedAt
	}
	http.ServeContent(w, r, path.Base(info.ID), modified, reader)
}

func (h *Handlers) handleRecordingPlay(w http.ResponseWriter, r *http.Request) {
	provider, ok := h.recordingProvider()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "recording module unavailable")
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing recording id")
		return
	}
	reader, info, err := provider.OpenRecording(r.Context(), id)
	if err != nil {
		writeRecordingError(w, err)
		return
	}
	defer reader.Close()
	if info.State != record.RecordingCompleted {
		writeRecordingError(w, record.ErrRecordingNotReady)
		return
	}
	w.Header().Set("Content-Type", recordingMediaType(info))
	if disposition := mime.FormatMediaType("inline", map[string]string{"filename": path.Base(info.ID)}); disposition != "" {
		w.Header().Set("Content-Disposition", disposition)
	}
	modified := info.CompletedAt
	if modified.IsZero() {
		modified = info.StartedAt
	}
	http.ServeContent(w, r, path.Base(info.ID), modified, reader)
}

func recordingMediaType(info record.RecordingInfo) string {
	switch strings.ToLower(info.Format) {
	case "flv":
		return "video/x-flv"
	case "mp4", "fmp4":
		return "video/mp4"
	case "ts", "hls":
		return "video/mp2t"
	}
	ext := strings.ToLower(path.Ext(info.ID))
	switch ext {
	case ".flv":
		return "video/x-flv"
	case ".mp4", ".m4v":
		return "video/mp4"
	case ".ts":
		return "video/mp2t"
	case ".m3u8":
		return "application/vnd.apple.mpegurl"
	}
	if mediaType := mime.TypeByExtension(ext); mediaType != "" {
		return mediaType
	}
	return "application/octet-stream"
}

func recordingHTTPStatus(err error) int {
	switch {
	case errors.Is(err, record.ErrInvalidRecordingID):
		return http.StatusBadRequest
	case errors.Is(err, record.ErrRecordingNotFound):
		return http.StatusNotFound
	case errors.Is(err, record.ErrRecordingNotReady):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

func writeRecordingError(w http.ResponseWriter, err error) {
	writeError(w, recordingHTTPStatus(err), err.Error())
}

type dvrStatusResponse struct {
	Enabled  bool                   `json:"enabled"`
	Sessions []dvr.DVRSessionStatus `json:"sessions"`
	Storage  dvr.DVRStorageHealth   `json:"storage"`
	Metrics  dvr.DVRMetricsSnapshot `json:"metrics"`
}

func (h *Handlers) dvrStatusProvider() (dvr.DVRStatusProvider, bool) {
	module := h.server.ModuleByName("dvr")
	provider, ok := module.(dvr.DVRStatusProvider)
	return provider, ok
}

func (h *Handlers) handleDVRStatus(w http.ResponseWriter, r *http.Request) {
	provider, ok := h.dvrStatusProvider()
	if !ok {
		writeJSON(w, http.StatusOK, dvrStatusResponse{Sessions: []dvr.DVRSessionStatus{}})
		return
	}
	status := provider.DVRStatus()
	writeJSON(w, http.StatusOK, dvrStatusResponse{
		Enabled: true, Sessions: status.Sessions, Storage: status.Storage, Metrics: status.Metrics,
	})
}

func (h *Handlers) handleDVRSession(w http.ResponseWriter, r *http.Request) {
	provider, ok := h.dvrStatusProvider()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "dvr module unavailable")
		return
	}
	streamKey := strings.TrimSpace(r.PathValue("stream_key"))
	if streamKey == "" {
		writeError(w, http.StatusBadRequest, "missing stream key")
		return
	}
	status, found := provider.DVRSession(streamKey)
	if !found {
		writeError(w, http.StatusNotFound, "dvr session not found")
		return
	}
	writeJSON(w, http.StatusOK, status)
}
