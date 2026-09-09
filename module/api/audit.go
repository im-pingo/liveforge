package api

import (
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// AuditEntry records one management-plane security decision or operation.
type AuditEntry struct {
	Time       time.Time         `json:"time"`
	RequestID  string            `json:"request_id"`
	Principal  string            `json:"principal"`
	Role       string            `json:"role"`
	Action     string            `json:"action"`
	Resource   string            `json:"resource"`
	Result     string            `json:"result"`
	RemoteAddr string            `json:"remote_addr,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
}

// AuditStore retains a bounded copy of recent audit entries.
type AuditStore struct {
	mu         sync.RWMutex
	maxEntries int
	entries    []AuditEntry
	start      int
	total      atomic.Uint64
	persistent *auditPersistence
	status     AuditPersistenceStatus
	closed     bool
	closeErr   error
}

func NewAuditStore(maxEntries int) *AuditStore {
	if maxEntries <= 0 {
		maxEntries = 1000
	}
	return &AuditStore{maxEntries: maxEntries, status: AuditPersistenceStatus{Healthy: true}}
}

func (s *AuditStore) Record(entry AuditEntry) {
	if entry.Time.IsZero() {
		entry.Time = time.Now().UTC()
	}
	entry = sanitizeAuditEntry(entry)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.retain(entry)
	if s.persistent != nil {
		if s.status.Error == "" {
			if err := s.persistent.append(entry); err != nil {
				s.status.Error = auditPersistenceError(err)
				s.status.Healthy = false
				slog.Error("audit persistence failed", "error", s.status.Error)
			}
		}
		if s.status.Error != "" {
			s.status.WriteFailures++
		}
	}
	s.total.Add(1)
	s.mu.Unlock()
	slog.Info("management audit", "request_id", entry.RequestID, "principal", entry.Principal,
		"role", entry.Role, "action", entry.Action, "resource", entry.Resource, "result", entry.Result)
}

// retain is called under mu or during startup before publication.
func (s *AuditStore) retain(entry AuditEntry) {
	if len(s.entries) < s.maxEntries {
		s.entries = append(s.entries, entry)
		return
	}
	s.entries[s.start] = entry
	s.start = (s.start + 1) % len(s.entries)
}

func (s *AuditStore) Total() uint64 { return s.total.Load() }

func (s *AuditStore) Entries() []AuditEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]AuditEntry, len(s.entries))
	for i := range out {
		out[i] = s.entries[(s.start+i)%len(s.entries)]
		out[i].Metadata = redactMetadata(out[i].Metadata)
	}
	return out
}

func redactMetadata(metadata map[string]string) map[string]string {
	if len(metadata) == 0 {
		return nil
	}
	out := make(map[string]string, len(metadata))
	for key, value := range metadata {
		if sensitiveAuditKey(key) {
			continue
		}
		out[key] = value
	}
	return out
}

func sensitiveAuditKey(key string) bool {
	lower := strings.ToLower(key)
	for _, part := range []string{"token", "secret", "password", "authorization", "credential", "api_key", "apikey", "cookie", "body", "payload", "document", "header", "content", "request", "response"} {
		if strings.Contains(lower, part) {
			return true
		}
	}
	return false
}

type AuditPersistenceStatus struct {
	Enabled       bool   `json:"enabled"`
	Healthy       bool   `json:"healthy"`
	Error         string `json:"error,omitempty"`
	WriteFailures uint64 `json:"write_failures_total"`
}

func (s *AuditStore) PersistenceStatus() AuditPersistenceStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.status
}

func (s *AuditStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return s.closeErr
	}
	s.closed = true
	if s.persistent != nil {
		s.closeErr = s.persistent.close()
		if s.closeErr != nil {
			s.status.Healthy = false
			s.status.Error = auditPersistenceError(s.closeErr)
			slog.Error("audit persistence close failed", "error", s.status.Error)
		}
	}
	return s.closeErr
}
