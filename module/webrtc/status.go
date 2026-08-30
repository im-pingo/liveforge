package webrtc

import (
	"encoding/json"
	"net/http"
	"time"
)

const (
	maxSessionStatusTombstones = 64
	sessionStatusTombstoneTTL  = 2 * time.Minute
)

type sessionStatusTombstone struct {
	status    sessionStatusResponse
	expiresAt time.Time
}

type sessionStatusResponse struct {
	SessionID string         `json:"session_id"`
	StreamKey string         `json:"stream_key"`
	Role      string         `json:"role"`
	Feed      WHEPFeedStatus `json:"feed"`
}

func (m *Module) handleStatus(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	if sess, active := m.findSession(sessionID); active {
		status, ok := sess.statusResponse()
		if !ok {
			http.Error(w, "session has no WHEP feed", http.StatusConflict)
			return
		}
		writeSessionStatus(w, status)
		return
	}
	status, ok := m.findSessionStatus(sessionID)
	if !ok {
		http.NotFound(w, r)
		return
	}
	writeSessionStatus(w, status)
}

func writeSessionStatus(w http.ResponseWriter, status sessionStatusResponse) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(status)
}

func (m *Module) findSessionStatus(sessionID string) (sessionStatusResponse, bool) {
	m.statusMu.Lock()
	defer m.statusMu.Unlock()
	m.pruneStatusTombstonesLocked(time.Now())
	tombstone, ok := m.statusTombstones[sessionID]
	if !ok {
		return sessionStatusResponse{}, false
	}
	return tombstone.status, true
}

func (m *Module) storeStatusTombstone(status sessionStatusResponse) {
	now := time.Now()
	m.statusMu.Lock()
	defer m.statusMu.Unlock()
	if m.statusTombstones == nil {
		m.statusTombstones = make(map[string]sessionStatusTombstone)
	}
	m.pruneStatusTombstonesLocked(now)
	if _, exists := m.statusTombstones[status.SessionID]; !exists {
		m.statusTombstoneOrder = append(m.statusTombstoneOrder, status.SessionID)
	}
	m.statusTombstones[status.SessionID] = sessionStatusTombstone{
		status:    status,
		expiresAt: now.Add(sessionStatusTombstoneTTL),
	}
	for len(m.statusTombstoneOrder) > maxSessionStatusTombstones {
		oldest := m.statusTombstoneOrder[0]
		m.statusTombstoneOrder = m.statusTombstoneOrder[1:]
		delete(m.statusTombstones, oldest)
	}
}

func (m *Module) pruneStatusTombstonesLocked(now time.Time) {
	for len(m.statusTombstoneOrder) > 0 {
		oldest := m.statusTombstoneOrder[0]
		tombstone, exists := m.statusTombstones[oldest]
		if exists && now.Before(tombstone.expiresAt) {
			break
		}
		m.statusTombstoneOrder = m.statusTombstoneOrder[1:]
		delete(m.statusTombstones, oldest)
	}
}
