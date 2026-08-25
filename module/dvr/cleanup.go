package dvr

import (
	"context"
	"log/slog"
	"time"
)

func (m *Module) runCleanup(ctx context.Context) {
	timer := time.NewTimer(m.cleanupInterval())
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-m.reloadCh:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(m.cleanupInterval())
		case <-timer.C:
			m.cleanExpiredSegments()
			timer.Reset(m.cleanupInterval())
		}
	}
}

func (m *Module) cleanupInterval() time.Duration {
	interval := m.Policy().CleanupInterval
	if interval <= 0 {
		return 30 * time.Second
	}
	return interval
}

func (m *Module) cleanExpiredSegments() {
	window := m.Policy().Window
	if window <= 0 {
		window = 2 * time.Hour
	}
	cutoff := time.Now().Add(-window)

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

		result := session.cleanBefore(cutoff)
		m.metrics.cleanupDeleted.Add(result.Deleted)
		m.metrics.cleanupBytes.Add(result.Bytes)
		m.metrics.cleanupFailures.Add(result.Failures)
		if result.Deleted > 0 || result.Failures > 0 {
			slog.Debug("dvr cleanup", "stream", key, "removed", result.Deleted, "failures", result.Failures)
		}

		if !session.IsLive() && session.Index().Len() == 0 {
			m.mu.Lock()
			if m.sessions[key] == session {
				delete(m.sessions, key)
				session.closeStorage()
			}
			m.mu.Unlock()
			slog.Info("dvr session expired", "module", "dvr", "stream", key)
		}
	}
}
