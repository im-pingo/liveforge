package runtime

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestDocumentHistoryBoundsCopiesAndRevisions(t *testing.T) {
	source := &writableMutableSource{snapshot: Snapshot{Data: []byte("server:\n  name: initial\n")}}
	m, err := NewManager(Options{Source: source, PollInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	original := m.Snapshot().DocumentRevision
	for i := 0; i < MaxDocumentHistoryEntries+2; i++ {
		if err := m.Write(context.Background(), []byte(fmt.Sprintf("# comment %d\nserver:\n  name: initial\n", i))); err != nil {
			t.Fatal(err)
		}
	}
	history := m.DocumentHistory()
	if len(history) != MaxDocumentHistoryEntries {
		t.Fatalf("history entries=%d", len(history))
	}
	if _, ok := m.HistoryDocument(original); ok {
		t.Fatal("evicted revision remains readable")
	}
	current := m.Snapshot().DocumentRevision
	document, ok := m.HistoryDocument(current)
	if !ok {
		t.Fatal("current revision unavailable")
	}
	document[0] = '!'
	copy, _ := m.HistoryDocument(current)
	if copy[0] != '#' {
		t.Fatal("caller mutated retained document")
	}
	if _, err := m.WriteIfRevision(context.Background(), copy, current); err != nil {
		t.Fatal(err)
	}
	if m.Snapshot().DocumentRevision != current {
		t.Fatal("identical write changed document revision")
	}
	if _, err := m.WriteIfRevision(context.Background(), []byte("server:\n  name: initial\n"), current); err != nil {
		t.Fatal(err)
	}
	if m.Snapshot().DocumentRevision == original {
		t.Fatal("returning to old content reused stale revision")
	}
}

func TestDocumentHistoryByteLimit(t *testing.T) {
	source := &writableMutableSource{snapshot: Snapshot{Data: []byte("server:\n  name: initial\n")}}
	m, err := NewManager(Options{Source: source, PollInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	padding := strings.Repeat("x", 2<<20)
	for i := 0; i < 10; i++ {
		if err := m.Write(context.Background(), []byte(fmt.Sprintf("# %d %s\nserver:\n  name: initial\n", i, padding))); err != nil {
			t.Fatal(err)
		}
	}
	bytes := 0
	for _, item := range m.DocumentHistory() {
		bytes += item.Bytes
	}
	if bytes > MaxDocumentHistoryBytes {
		t.Fatalf("history retained%d bytes", bytes)
	}
}
