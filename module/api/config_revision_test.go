package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	configruntime "github.com/im-pingo/liveforge/config/runtime"
	"github.com/im-pingo/liveforge/core"
)

func configRevisionFixture(t *testing.T) (*Handlers, *core.Server, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("# source comment\nserver:\n  name: original\napi:\n  console:\n    username: admin\n    password: history-test-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := configruntime.NewFileSource(path)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := configruntime.NewManager(configruntime.Options{Source: source, PollInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { manager.Close() })
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	h, server := newTestHandlers(t)
	server.SetConfigManager(manager)
	return h, server, path
}

func desiredRevision(t *testing.T, h *Handlers) string {
	t.Helper()
	w := httptest.NewRecorder()
	h.handleConfigDocument(w, httptest.NewRequest(http.MethodGet, "/api/v1/server/config/document", nil))
	var doc struct {
		Revision string `json:"document_revision"`
	}
	if err := json.Unmarshal(decodeAPIData(t, w.Body.Bytes()), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Revision == "" || w.Header().Get("ETag") != `"`+doc.Revision+`"` {
		t.Fatalf("missing document revision or ETag: revision=%q ETag=%q", doc.Revision, w.Header().Get("ETag"))
	}
	return doc.Revision
}

func TestConfigApplyRejectsStaleRevisionWithoutWriting(t *testing.T) {
	h, _, path := configRevisionFixture(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/server/config/apply", strings.NewReader("server:\n  name: stale-write\n"))
	r.Header.Set("If-Match", `"stale-revision"`)
	w := httptest.NewRecorder()
	h.handleConfigApply(w, r)
	if w.Code != http.StatusConflict {
		t.Fatalf("stale apply status=%d body=%s", w.Code, w.Body.String())
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatalf("stale apply modified source: err=%v", err)
	}
}

func TestConfigRevisionIncludesCommentsAndConcurrentApplyIsAtomic(t *testing.T) {
	h, _, path := configRevisionFixture(t)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	revision := desiredRevision(t, h)
	responses := make(chan int, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, name := range []string{"first", "second"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r := httptest.NewRequest(http.MethodPost, "/api/v1/server/config/apply", strings.NewReader("# "+name+"\n"+string(original)))
			r.Header.Set("If-Match", `"`+revision+`"`)
			w := httptest.NewRecorder()
			h.handleConfigApply(w, r)
			responses <- w.Code
		}()
	}
	close(start)
	wg.Wait()
	close(responses)
	counts := make(map[int]int)
	for status := range responses {
		counts[status]++
	}
	if counts[http.StatusAccepted] != 1 || counts[http.StatusConflict] != 1 {
		t.Fatalf("concurrent apply statuses=%v want one202 and one409", counts)
	}
	if next := desiredRevision(t, h); next == revision {
		t.Fatal("desired revision did not change")
	}
}

func TestConfigRollbackRequiresCurrentRevision(t *testing.T) {
	h, _, path := configRevisionFixture(t)
	revision := desiredRevision(t, h)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		header string
		status int
	}{
		{"", http.StatusPreconditionRequired},
		{`"stale"`, http.StatusConflict},
		{`W/"weak"`, http.StatusBadRequest},
	} {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/server/config/rollback", strings.NewReader(`{"revision":"`+revision+`"}`))
		r.Header.Set("If-Match", tc.header)
		w := httptest.NewRecorder()
		h.handleConfigRollback(w, r)
		if w.Code != tc.status {
			t.Fatalf("If-Match=%q status=%d want%d", tc.header, w.Code, tc.status)
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatal("rejected rollback modified source")
	}
}

func TestConfigHistoryRedactsSecretsAndRollbackUsesPrecondition(t *testing.T) {
	h, server, path := configRevisionFixture(t)
	mux := http.NewServeMux()
	registerRoutes(mux, server, nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/server/config/history", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("history status=%d", w.Code)
	}
	original := desiredRevision(t, h)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/server/config/apply", strings.NewReader("server:\n  name: updated\n"))
	r.Header.Set("If-Match", `"`+original+`"`)
	w = httptest.NewRecorder()
	h.handleConfigApply(w, r)
	if w.Code != http.StatusAccepted {
		t.Fatalf("apply status=%d body=%s", w.Code, w.Body.String())
	}
	current := desiredRevision(t, h)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/server/config/history/"+original, nil))
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "history-test-secret") || !strings.Contains(w.Body.String(), "REDACTED") {
		t.Fatalf("history must be available and redacted: status=%d", w.Code)
	}
	r = httptest.NewRequest(http.MethodPost, "/api/v1/server/config/rollback", strings.NewReader(`{"revision":"`+original+`"}`))
	r.Header.Set("If-Match", `"`+current+`"`)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusAccepted {
		t.Fatalf("rollback status=%d body=%s", w.Code, w.Body.String())
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), "name: original") || !strings.Contains(string(data), "history-test-secret") {
		t.Fatalf("rollback failed to restore source and original secret: err=%v", err)
	}
}
