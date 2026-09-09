package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"gopkg.in/yaml.v3"
)

func TestModuleAuditPersistenceRestoresRetainedEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.ndjson")
	cfg := newTestConfig()
	if err := yaml.Unmarshal([]byte(fmt.Sprintf("api:\n  listen: 127.0.0.1:0\n  audit:\n    path: %q\n    max_entries: 2\n    max_bytes: 65536\n", path)), cfg); err != nil {
		t.Fatal(err)
	}
	first := NewModule()
	if err := first.Init(core.NewServer(cfg)); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"first", "second", "third"} {
		first.audit.Record(AuditEntry{Principal: name, Action: "config:apply"})
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second := NewModule()
	if err := second.Init(core.NewServer(cfg)); err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	entries := second.audit.Entries()
	if len(entries) != 2 || entries[0].Principal != "second" || entries[1].Principal != "third" {
		t.Fatalf("restored audit entries = %+v", entries)
	}
}

func TestAuditEntryBoundaryRedactsURLsAndBodies(t *testing.T) {
	store := NewAuditStore(4)
	store.Record(AuditEntry{ // #nosec G101 -- Fake credential URLs exercise audit redaction.
		Resource: "https://alice:url-password@example.test/private-token?token=query-token#fragment-token",
		Metadata: map[string]string{ // #nosec G101 -- Fake credential URLs exercise audit redaction.
			"body": "raw-body-token", "request_headers": "Authorization: header-token", "api_key": "api-key-token",
			"endpoint": "rtsp://alice:rtsp-password@example.test/private-path?key=rtsp-key",
			"reason":   "approved",
		},
	})
	data, err := json.Marshal(store.Entries())
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"url-password", "private-token", "query-token", "fragment-token", "raw-body-token", "header-token", "api-key-token", "rtsp-password", "private-path", "rtsp-key"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("audit boundary retained %q: %s", secret, data)
		}
	}
	if store.Entries()[0].Metadata["reason"] != "approved" {
		t.Fatal("safe metadata was lost")
	}
}

func TestAuditEntryBoundaryHandlesNetworkURLsAndIPv6(t *testing.T) {
	store := NewAuditStore(4)
	store.Record(AuditEntry{RemoteAddr: "[2001:db8::1]:54321", Metadata: map[string]string{
		"endpoint": "//user:network-secret@example.test/private-credential",
		"reason":   "password=inline-secret",
		"content":  "raw-content-secret",
	}})
	entry := store.Entries()[0]
	if entry.RemoteAddr != "[2001:db8::1]:54321" {
		t.Errorf("ordinary IPv6 peer changed: %q", entry.RemoteAddr)
	}
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"network-secret", "private-credential", "inline-secret", "raw-content-secret"} {
		if bytes.Contains(data, []byte(secret)) {
			t.Errorf("audit retained %q", secret)
		}
	}
}

func TestAuditPersistenceRotatesAndRestoresChronologicalHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.ndjson")
	cfg := auditTestConfig(path)
	store, err := OpenAuditStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 48 {
		metadata := make(map[string]string)
		for j := range 16 {
			metadata[fmt.Sprintf("field_%d", j)] = strings.Repeat("x", 256)
		}
		store.Record(AuditEntry{Principal: fmt.Sprintf("actor-%d", i), Metadata: metadata})
	}
	want := store.Entries()
	if closeErr := store.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	for _, name := range []string{path, path + ".1", path + ".lock"} {
		info, statErr := os.Lstat(name)
		if statErr != nil {
			t.Fatal(statErr)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 65536 {
			t.Fatalf("unsafe or oversized audit file %q: %v", name, info)
		}
	}
	files, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(files) != 3 {
		t.Fatalf("unbounded rotation files: %v, err=%v", files, err)
	}
	restored, err := OpenAuditStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if got := restored.Entries(); !reflect.DeepEqual(got, want) {
		t.Fatalf("restored history mismatch: got=%+v want=%+v", got, want)
	}
	if restored.Total() != 0 {
		t.Fatal("startup replay incremented process audit counters")
	}
}

func TestAuditPersistenceRecoversTruncatedCrashLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.ndjson")
	cfg := auditTestConfig(path)
	store, err := OpenAuditStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	store.Record(AuditEntry{Principal: "before-crash"})
	if closeErr := store.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, writeErr := file.WriteString(`{"principal":"incomplete`); writeErr != nil {
		t.Fatal(writeErr)
	}
	file.Close()
	store, err = OpenAuditStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if entries := store.Entries(); len(entries) != 1 || entries[0].Principal != "before-crash" {
		t.Fatalf("crash recovery history = %+v", entries)
	}
	store.Record(AuditEntry{Principal: "after-crash"})
	if closeErr := store.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	data, err := os.ReadFile(path)
	if err != nil || bytes.Contains(data, []byte("incomplete")) || bytes.Count(data, []byte{'\n'}) != 2 {
		t.Fatalf("crash tail was not repaired: data=%s err=%v", data, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	for range 2 {
		var entry AuditEntry
		if err := decoder.Decode(&entry); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAuditPersistenceRejectsUnsafeAndUnboundedFiles(t *testing.T) {
	for _, test := range []struct {
		name string
		make func(*testing.T, string)
	}{
		{name: "primary symlink", make: func(t *testing.T, path string) { auditTestSymlink(t, path) }},
		{name: "rotated symlink", make: func(t *testing.T, path string) { auditTestSymlink(t, path+".1") }},
		{name: "lock symlink", make: func(t *testing.T, path string) { auditTestSymlink(t, path+".lock") }},
		{name: "directory", make: func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "permissions", make: func(t *testing.T, path string) {
			if err := os.WriteFile(path, nil, 0644); err != nil { // #nosec G306 -- Verify rejection of an intentionally unsafe audit file.
				t.Fatal(err)
			}
		}},
		{name: "hard link", make: func(t *testing.T, path string) {
			if err := os.WriteFile(path+".other", nil, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(path+".other", path); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "oversized file", make: func(t *testing.T, path string) {
			if err := os.WriteFile(path, bytes.Repeat([]byte{'x'}, 65537), 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "invalid complete entry", make: func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("{invalid}\n"), 0600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "audit.ndjson")
			test.make(t, path)
			store, err := OpenAuditStore(auditTestConfig(path))
			if err == nil {
				store.Close()
				t.Fatal("unsafe audit storage was accepted")
			}
		})
	}
}

func TestAuditPersistenceRejectsConcurrentWriterAndReleasesOnClose(t *testing.T) {
	cfg := auditTestConfig(filepath.Join(t.TempDir(), "audit.ndjson"))
	store, err := OpenAuditStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if duplicate, duplicateErr := OpenAuditStore(cfg); duplicateErr == nil {
		duplicate.Close()
		t.Fatal("second audit writer acquired the same storage")
	}
	if closeErr := store.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if closeErr := store.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	next, err := OpenAuditStore(cfg)
	if err != nil {
		t.Fatalf("close left audit lock held: %v", err)
	}
	next.Close()
}

func TestAuditPersistenceReportsWriteFailureAndKeepsMemoryTrail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private-audit-path.ndjson")
	store, err := OpenAuditStore(auditTestConfig(path))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	store.Record(AuditEntry{Principal: "one"})
	store.Record(AuditEntry{Principal: "two"})
	if entries := store.Entries(); len(entries) != 2 || entries[0].Principal != "one" || entries[1].Principal != "two" {
		t.Fatalf("disk failure lost memory trail: %+v", entries)
	}
	handler := newHandlersWithAudit(core.NewServer(newTestConfig()), store)
	response := httptest.NewRecorder()
	handler.handleSecurityStatus(response, httptest.NewRequest(http.MethodGet, "/api/v1/security/status", nil))
	var result struct {
		Data struct {
			Persistence struct {
				Enabled  bool   `json:"enabled"`
				Healthy  bool   `json:"healthy"`
				Error    string `json:"error"`
				Failures uint64 `json:"write_failures_total"`
			} `json:"audit_persistence"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	status := result.Data.Persistence
	if !status.Enabled || status.Healthy || status.Error == "" || status.Failures != 2 || strings.Contains(status.Error, "private-audit-path") {
		t.Fatalf("persistence failure status = %+v", status)
	}
}

func TestAuditPersistenceBoundsLargeEntriesBeforeMemoryAndDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.ndjson")
	store, err := OpenAuditStore(auditTestConfig(path))
	if err != nil {
		t.Fatal(err)
	}
	metadata := make(map[string]string)
	for i := range 100 {
		metadata[fmt.Sprintf("field_%d", i)] = strings.Repeat("\x00", 4096)
	}
	store.Record(AuditEntry{Resource: strings.Repeat("x", 1<<20), Principal: strings.Repeat("p", 1<<20), Metadata: metadata})
	entries := store.Entries()
	if len(entries) != 1 || len(entries[0].Resource) > 2048 || len(entries[0].Principal) > 512 || len(entries[0].Metadata) > 16 {
		t.Fatal("audit memory entry was not bounded")
	}
	if closeErr := store.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) > 65536 || !json.Valid(bytes.TrimSpace(data)) {
		t.Fatalf("persisted audit entry: bytes=%d err=%v", len(data), err)
	}
}

func TestAuditPersistenceConcurrentRecordAndRead(t *testing.T) {
	cfg := auditTestConfig(filepath.Join(t.TempDir(), "audit.ndjson"))
	store, err := OpenAuditStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for worker := range 4 {
		workers.Go(func() {
			for i := range 40 {
				store.Record(AuditEntry{Principal: fmt.Sprintf("worker-%d-%d", worker, i)})
				store.Entries()
				store.PersistenceStatus()
			}
		})
	}
	workers.Wait()
	want := store.Entries()
	if store.Total() != 160 {
		t.Fatalf("concurrent audit count = %d", store.Total())
	}
	if closeErr := store.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	restored, err := OpenAuditStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if got := restored.Entries(); !reflect.DeepEqual(got, want) {
		t.Fatalf("concurrent disk order differs from memory: got=%+v want=%+v", got, want)
	}
}

func TestAuditPersistenceReplayRedactsAndRejectsOversizedLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.ndjson")
	cfg := auditTestConfig(path)
	if err := os.WriteFile(path, []byte("{\"resource\":\"https://user:replay-secret@example.test/x?token=replay-query\",\"metadata\":{\"body\":\"replay-body\"}}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := OpenAuditStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(store.Entries())
	if err != nil || bytes.Contains(data, []byte("replay-")) {
		t.Fatalf("startup replay was not redacted: %s err=%v", data, err)
	}
	store.Close()
	if writeErr := os.WriteFile(path, bytes.Repeat([]byte{'x'}, 65537), 0600); writeErr != nil {
		t.Fatal(writeErr)
	}
	cfg.MaxBytes = 128 << 10
	if oversized, oversizedErr := OpenAuditStore(cfg); oversizedErr == nil {
		oversized.Close()
		t.Fatal("startup accepted an oversized line within file size limit")
	}
	if writeErr := os.WriteFile(path, nil, 0600); writeErr != nil {
		t.Fatal(writeErr)
	}
	repaired, err := OpenAuditStore(cfg)
	if err != nil {
		t.Fatalf("failed initialization leaked the writer lock: %v", err)
	}
	repaired.Close()
}

func TestModuleAuditPersistenceRejectsUnusableStorage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.ndjson")
	auditTestSymlink(t, path)
	cfg := newTestConfig()
	cfg.API.Audit = auditTestConfig(path)
	cfg.API.Listen = "127.0.0.1:0"
	module := NewModule()
	if err := module.Init(core.NewServer(cfg)); err == nil {
		module.Close()
		t.Fatal("API initialized with unusable configured audit storage")
	}
	if module.Addr() != nil {
		t.Fatal("API listener was opened before audit storage validation")
	}
}

func TestModuleAuditPersistenceReleasesStorageWhenListenerFails(t *testing.T) {
	cfg := newTestConfig()
	cfg.API.Audit = auditTestConfig(filepath.Join(t.TempDir(), "audit.ndjson"))
	cfg.API.Listen = "invalid listener"
	module := NewModule()
	if err := module.Init(core.NewServer(cfg)); err == nil {
		module.Close()
		t.Fatal("invalid listener unexpectedly initialized")
	}
	store, err := OpenAuditStore(cfg.API.Audit)
	if err != nil {
		t.Fatalf("failed API initialization leaked the audit writer: %v", err)
	}
	store.Close()
}

func TestModuleAuditPersistenceDrainsRequestsBeforeClose(t *testing.T) {
	cfg := auditTestConfig(filepath.Join(t.TempDir(), "audit.ndjson"))
	store, err := OpenAuditStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		store.Record(AuditEntry{Principal: "final-request"})
		w.WriteHeader(http.StatusNoContent)
	}))
	shutdownStarted := make(chan struct{})
	server.Config.RegisterOnShutdown(func() { close(shutdownStarted) })
	server.Start()
	defer server.Close()
	module := &Module{httpSrv: server.Config, audit: store}
	requestDone := make(chan error, 1)
	go func() {
		response, requestErr := server.Client().Get(server.URL)
		if requestErr == nil {
			response.Body.Close()
		}
		requestDone <- requestErr
	}()
	<-entered
	closeDone := make(chan error, 1)
	go func() { closeDone <- module.Close() }()
	select {
	case <-shutdownStarted:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("API close did not begin a graceful request drain")
	}
	select {
	case closeErr := <-closeDone:
		close(release)
		t.Fatalf("audit closed before the active request completed: %v", closeErr)
	default:
	}
	close(release)
	if closeErr := <-closeDone; closeErr != nil {
		t.Fatal(closeErr)
	}
	if requestErr := <-requestDone; requestErr != nil {
		t.Fatal(requestErr)
	}
	restored, err := OpenAuditStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if entries := restored.Entries(); len(entries) != 1 || entries[0].Principal != "final-request" {
		t.Fatalf("shutdown lost an admitted request audit: %+v", entries)
	}
}

func TestAuditPersistenceClosePropagatesSyncFailure(t *testing.T) {
	store, err := OpenAuditStore(auditTestConfig(filepath.Join(t.TempDir(), "audit.ndjson")))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.persistent.file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("close error = %v", err)
	}
	if status := store.PersistenceStatus(); status.Healthy || status.Error == "" {
		t.Fatalf("close failure was not reported: %+v", status)
	}
}

func TestAuditPersistenceErrorDoesNotExposeFilePaths(t *testing.T) {
	for _, err := range []error{
		&os.PathError{Op: "write", Path: "/private/audit.ndjson", Err: os.ErrPermission},
		&os.LinkError{Op: "rename", Old: "/private/audit.ndjson", New: "/private/audit.ndjson.1", Err: os.ErrPermission},
	} {
		message := auditPersistenceError(fmt.Errorf("persist audit: %w", err))
		if strings.Contains(message, "/private/") || !strings.Contains(message, os.ErrPermission.Error()) {
			t.Errorf("persistence error exposed a path or lost the cause: %q", message)
		}
	}
}

func auditTestSymlink(t *testing.T, path string) {
	t.Helper()
	target := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(target, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		file, err := os.Open(target)
		if err != nil {
			t.Error(err)
			return
		}
		defer file.Close()
		data, err := io.ReadAll(file)
		if err != nil || string(data) != "private" {
			t.Errorf("unsafe path changed its target: %q err=%v", data, err)
		}
	})
}

func auditTestConfig(path string) config.AuditConfig {
	var cfg config.AuditConfig
	if err := yaml.Unmarshal([]byte(fmt.Sprintf("path: %q\nmax_entries: 4\nmax_bytes: 65536\n", path)), &cfg); err != nil {
		panic(err)
	}
	return cfg
}
