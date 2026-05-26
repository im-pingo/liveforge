package dvr

import (
	"context"
	"log/slog"
	"time"
)

func (m *Module) runCleanup(ctx context.Context) {
	interval := m.cfg.CleanupInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.cleanExpiredSegments()
		}
	}
}

func (m *Module) cleanExpiredSegments() {
	cutoff := time.Now().Add(-m.cfg.Window)

	m.mu.Lock()
	keys := make([]string, 0, len(m.sessions))
	for k := range m.sessions {
		keys = append(keys, k)
	}
	m.mu.Unlock()

	for _, key := range keys {
		m.mu.Lock()
		session := m.sessions[key]
		m.mu.Unlock()

		if session == nil {
			continue
		}

		removed := session.Index().CleanBefore(cutoff)
		if len(removed) > 0 {
			slog.Debug("dvr cleanup", "stream", key, "removed", len(removed))
		}

		if !session.IsLive() && session.Index().Len() == 0 {
			m.mu.Lock()
			if m.sessions[key] == session {
				delete(m.sessions, key)
			}
			m.mu.Unlock()
			slog.Info("dvr session expired", "module", "dvr", "stream", key)
		}
	}
}
