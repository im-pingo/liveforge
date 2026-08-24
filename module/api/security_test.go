package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
)

func TestRBACRoleMatrix(t *testing.T) {
	tests := []struct {
		role, permission string
		want             bool
	}{
		{"viewer", "streams:read", true},
		{"viewer", "streams:kick", false},
		{"operator", "streams:kick", true},
		{"operator", "streams:delete", false},
		{"admin", "streams:delete", true},
		{"admin", "recordings:delete", true},
		{"unknown", "streams:read", false},
	}
	for _, tt := range tests {
		if got := roleAllows(tt.role, tt.permission); got != tt.want {
			t.Errorf("roleAllows(%q, %q) = %v, want %v", tt.role, tt.permission, got, tt.want)
		}
	}
}

func TestSecurityHandlerEnforcesPermissionAndAuditsDenial(t *testing.T) {
	cfg := config.Defaults()
	cfg.API.Auth.Tokens = []config.APIAuthToken{{Name: "readonly", Token: "view-token", Role: "viewer"}}
	server := core.NewServer(cfg)
	audit := NewAuditStore(16)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	handler := buildSecurityHandler(next, server, audit)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/streams/live/test", nil)
	req.Header.Set("Authorization", "Bearer view-token")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	entries := audit.Entries()
	if len(entries) != 1 || entries[0].Result != "denied" || entries[0].Principal != "readonly" || entries[0].Action != "streams:delete" {
		t.Fatalf("audit entries = %#v", entries)
	}
}

func TestViewerCannotDeleteRecordingAndDenialIsAudited(t *testing.T) {
	cfg := config.Defaults()
	cfg.API.Auth.Tokens = []config.APIAuthToken{{Name: "readonly", Token: "view-token", Role: "viewer"}}
	server := core.NewServer(cfg)
	audit := NewAuditStore(16)
	handler := buildSecurityHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), server, audit)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/recordings/live/cam.flv", nil)
	req.Header.Set("Authorization", "Bearer view-token")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d want=403", w.Code)
	}
	entries := audit.Entries()
	if len(entries) != 1 || entries[0].Action != "recordings:delete" || entries[0].Result != "denied" {
		t.Fatalf("audit entries=%#v", entries)
	}
}

func TestSecurityHandlerReadsReloadedBearerToken(t *testing.T) {
	cfg := config.Defaults()
	cfg.API.Auth.BearerToken = "old-token"
	server := core.NewServer(cfg)
	handler := buildSecurityHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }), server, NewAuditStore(4))

	next := *cfg
	next.API.Auth.BearerToken = "new-token"
	server.UpdateConfig(&next)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/streams", nil)
	req.Header.Set("Authorization", "Bearer new-token")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("new token status = %d, want 200", w.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/streams", nil)
	req.Header.Set("Authorization", "Bearer old-token")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("old token status = %d, want 401", w.Code)
	}
}

func TestSecurityMetricsCountAuthenticationAndAuthorizationFailures(t *testing.T) {
	cfg := config.Defaults()
	cfg.API.Auth.Tokens = []config.APIAuthToken{{Name: "readonly", Token: "view-token", Role: "viewer"}}
	server := core.NewServer(cfg)
	counters := &SecurityCounters{}
	handler := buildSecurityHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }), server, NewAuditStore(4), counters)

	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/api/v1/streams", nil),
		httptest.NewRequest(http.MethodDelete, "/api/v1/streams/live/test", nil),
	} {
		if req.Method == http.MethodDelete {
			req.Header.Set("Authorization", "Bearer view-token")
		}
		handler.ServeHTTP(httptest.NewRecorder(), req)
	}
	got := counters.Snapshot()
	if got.AuthenticationFailures != 1 || got.AuthorizationFailures != 1 {
		t.Fatalf("metrics = %#v", got)
	}
}

func TestAuditStoreRedactsSecretsAndBoundsEntries(t *testing.T) {
	store := NewAuditStore(2)
	store.Record(AuditEntry{Principal: "a", Metadata: map[string]string{"token": "secret", "stream": "one"}})
	store.Record(AuditEntry{Principal: "b"})
	store.Record(AuditEntry{Principal: "c"})
	entries := store.Entries()
	if len(entries) != 2 || entries[0].Principal != "b" {
		t.Fatalf("entries = %#v", entries)
	}
	for _, entry := range entries {
		if entry.Metadata["token"] != "" {
			t.Fatalf("secret metadata was retained: %#v", entry.Metadata)
		}
	}
}

func TestAuditEndpointReturnsRedactedEntries(t *testing.T) {
	h, _ := newTestHandlers(t)
	h.audit = NewAuditStore(4)
	h.audit.Record(AuditEntry{Principal: "admin", Action: "streams:delete", Metadata: map[string]string{"password": "bad", "reason": "test"}})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/audit", nil)
	w := httptest.NewRecorder()
	h.handleAudit(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var envelope struct {
		Data []AuditEntry `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Data) != 1 || envelope.Data[0].Metadata["password"] != "" || envelope.Data[0].Metadata["reason"] != "test" {
		t.Fatalf("entries = %#v", envelope.Data)
	}
}

func TestConfigRefreshWithoutManagerIsUnavailable(t *testing.T) {
	h, _ := newTestHandlers(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/server/config/refresh", nil)
	w := httptest.NewRecorder()
	h.handleConfigRefresh(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}

func TestSecurityStatusDoesNotExposeTokenValues(t *testing.T) {
	cfg := newTestConfig()
	cfg.API.Auth.BearerToken = "legacy-secret"
	cfg.API.Auth.Tokens = []config.APIAuthToken{{Name: "ops", Token: "named-secret", Role: "operator"}}
	server := core.NewServer(cfg)
	h := NewHandlers(server)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/security/status", nil)
	w := httptest.NewRecorder()
	h.handleSecurityStatus(w, req)
	body := w.Body.String()
	if w.Code != http.StatusOK || !strings.Contains(body, `"name":"ops"`) || strings.Contains(body, "legacy-secret") || strings.Contains(body, "named-secret") {
		t.Fatalf("status=%d body=%s", w.Code, body)
	}
}

func TestManagementAPIStaysAnonymousWhenOnlyConsoleLoginIsConfigured(t *testing.T) {
	cfg := config.Defaults()
	cfg.API.Console.Username = "admin"
	cfg.API.Console.Password = "admin"
	server := core.NewServer(cfg)
	handler := buildSecurityHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), server, NewAuditStore(4))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/streams", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want=200 body=%s", w.Code, w.Body.String())
	}
}

func TestViewerCannotAccessDebugEndpoints(t *testing.T) {
	cfg := config.Defaults()
	cfg.API.Auth.Tokens = []config.APIAuthToken{{Name: "readonly", Token: "view-token", Role: "viewer"}}
	server := core.NewServer(cfg)
	handler := buildSecurityHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), server, NewAuditStore(4))
	req := httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil)
	req.Header.Set("Authorization", "Bearer view-token")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d want=403", w.Code)
	}
}

func TestConsoleLoginFailureCountsAndAuditsAuthenticationFailure(t *testing.T) {
	cfg := config.Defaults()
	cfg.API.Console.Username = "admin"
	cfg.API.Console.Password = "correct"
	server := core.NewServer(cfg)
	audit := NewAuditStore(4)
	counters := &SecurityCounters{}
	handler := buildSecurityHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}), server, audit, counters)
	req := httptest.NewRequest(http.MethodPost, "/console/login", strings.NewReader("username=admin&password=wrong"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want=401", w.Code)
	}
	if got := counters.Snapshot().AuthenticationFailures; got != 1 {
		t.Fatalf("authentication failures=%d want=1", got)
	}
	entries := audit.Entries()
	if len(entries) != 1 || entries[0].Action != "console:login" || entries[0].Result != "unauthorized" {
		t.Fatalf("audit entries=%#v", entries)
	}
}
