package runtime

import (
	"bytes"
	"crypto/rand"
	"time"
)

const (
	MaxDocumentHistoryEntries = 32
	MaxDocumentHistoryBytes   = 16 << 20
)

// DocumentHistoryEntry describes a document retained by this process. Original
// documents are private to the manager; HTTP handlers must redact any copy.
type DocumentHistoryEntry struct {
	Revision  string    `json:"revision"`
	CreatedAt time.Time `json:"created_at"`
	Bytes     int       `json:"bytes"`
}

type documentHistoryItem struct {
	DocumentHistoryEntry
	document []byte
}

// assignDocumentRevision is called under publishMu before snapshot publication.
func (m *Manager) assignDocumentRevision(next *ConfigSnapshot) {
	if current := m.active.Load(); current != nil && current.DocumentRevision != "" && bytes.Equal(current.DesiredDocument, next.DesiredDocument) {
		next.DocumentRevision = current.DocumentRevision
		return
	}
	next.DocumentRevision = rand.Text()
	if len(next.DesiredDocument) > MaxDocumentHistoryBytes {
		return
	}
	item := documentHistoryItem{
		DocumentHistoryEntry: DocumentHistoryEntry{Revision: next.DocumentRevision, CreatedAt: time.Now().UTC(), Bytes: len(next.DesiredDocument)},
		document:             append([]byte(nil), next.DesiredDocument...),
	}
	m.history = append(m.history, item)
	m.historyBytes += item.Bytes
	for len(m.history) > MaxDocumentHistoryEntries || m.historyBytes > MaxDocumentHistoryBytes {
		m.historyBytes -= m.history[0].Bytes
		m.history[0] = documentHistoryItem{}
		m.history = m.history[1:]
	}
}

// DocumentHistory returns newest-first metadata without secret content.
func (m *Manager) DocumentHistory() []DocumentHistoryEntry {
	m.publishMu.Lock()
	defer m.publishMu.Unlock()
	entries := make([]DocumentHistoryEntry, len(m.history))
	for i, item := range m.history {
		entries[len(entries)-1-i] = item.DocumentHistoryEntry
	}
	return entries
}

// HistoryDocument returns an owned copy, suitable for validated rollback or
// redaction. Unknown or evicted revisions are not available.
func (m *Manager) HistoryDocument(revision string) ([]byte, bool) {
	m.publishMu.Lock()
	defer m.publishMu.Unlock()
	for _, item := range m.history {
		if item.Revision == revision {
			return append([]byte(nil), item.document...), true
		}
	}
	return nil, false
}
