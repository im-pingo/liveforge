package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"time"
)

func (h *Handlers) handleAudit(w http.ResponseWriter, r *http.Request) {
	page, err := readPagination(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	q := r.URL.Query()
	var since, until time.Time
	for name, target := range map[string]*time.Time{"since": &since, "until": &until} {
		if value := q.Get(name); value != "" {
			parsed, err := time.Parse(time.RFC3339Nano, value)
			if err != nil {
				writeError(w, http.StatusBadRequest, name+" must be RFC3339")
				return
			}
			*target = parsed
		}
	}
	if !since.IsZero() && !until.IsZero() && since.After(until) {
		writeError(w, http.StatusBadRequest, "since must not exceed until")
		return
	}
	if order := q.Get("order"); order != "" && order != "asc" && order != "desc" {
		writeError(w, http.StatusBadRequest, "order must be asc or desc")
		return
	}
	if format := q.Get("format"); format != "" && format != "json" && format != "ndjson" {
		writeError(w, http.StatusBadRequest, "format must be json or ndjson")
		return
	}
	entries := make([]AuditEntry, 0)
	for _, entry := range h.audit.Entries() {
		if (q.Get("principal") != "" && entry.Principal != q.Get("principal")) || (q.Get("action") != "" && entry.Action != q.Get("action")) || (q.Get("result") != "" && entry.Result != q.Get("result")) || (!since.IsZero() && entry.Time.Before(since)) || (!until.IsZero() && entry.Time.After(until)) {
			continue
		}
		entries = append(entries, entry)
	}
	if q.Get("order") == "desc" {
		slices.Reverse(entries)
	}
	entries = pageItems(entries, page)
	if q.Get("format") == "ndjson" {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Content-Disposition", `attachment; filename="liveforge-audit.ndjson"`)
		encoder := json.NewEncoder(w)
		for _, entry := range entries {
			if encoder.Encode(entry) != nil {
				return
			}
		}
		return
	}
	writePageJSON(w, entries, page)
}
