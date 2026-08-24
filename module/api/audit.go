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
	total      atomic.Uint64
}

func NewAuditStore(maxEntries int) *AuditStore {
	if maxEntries <= 0 {
		maxEntries = 1000
	}
	return &AuditStore{maxEntries: maxEntries}
}

func (s *AuditStore) Record(entry AuditEntry) {
	if entry.Time.IsZero() {
		entry.Time = time.Now().UTC()
	}
	entry.Metadata = redactMetadata(entry.Metadata)
	s.mu.Lock()
	s.entries = append(s.entries, entry)
	if excess := len(s.entries) - s.maxEntries; excess > 0 {
		copy(s.entries, s.entries[excess:])
		s.entries = s.entries[:s.maxEntries]
	}
	s.mu.Unlock()
	s.total.Add(1)
	slog.Info("management audit", "request_id", entry.RequestID, "principal", entry.Principal,
		"role", entry.Role, "action", entry.Action, "resource", entry.Resource, "result", entry.Result)
}

func (s *AuditStore) Total() uint64 { return s.total.Load() }

func (s *AuditStore) Entries() []AuditEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]AuditEntry, len(s.entries))
	copy(out, s.entries)
	for i := range out {
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
		lower := strings.ToLower(key)
		if strings.Contains(lower, "token") || strings.Contains(lower, "secret") || strings.Contains(lower, "password") || strings.Contains(lower, "authorization") {
			continue
		}
		out[key] = value
	}
	return out
}
