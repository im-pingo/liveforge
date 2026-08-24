package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/im-pingo/liveforge/config"
	configruntime "github.com/im-pingo/liveforge/config/runtime"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/module/record"
	"github.com/im-pingo/liveforge/module/sipgateway"
)

type securityConfigSource struct{}

func (securityConfigSource) Load(ctx context.Context, _ configruntime.Version) (configruntime.Snapshot, error) {
	<-ctx.Done()
	return configruntime.Snapshot{}, ctx.Err()
}
func (securityConfigSource) Close() error { return nil }

func TestRBACRoleMatrix(t *testing.T) {
	tests := []struct {
		role, permission string
		want             bool
	}{
		{"viewer", "streams:read", true},
		{"viewer", "streams:kick", false},
		{"operator", "streams:kick", true},
		{"operator", "gb28181:read", true},
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

func TestConfigApplyAuditIsRecordedOnlyByPostCommitHook(t *testing.T) {
	cfg := newTestConfig()
	server := core.NewServer(cfg)
	m := NewModule()
	m.audit = NewAuditStore(4)
	if err := m.OnReload(server); err != nil {
		t.Fatal(err)
	}
	if entries := m.audit.Entries(); len(entries) != 0 {
		t.Fatalf("reload preparation emitted success audit: %#v", entries)
	}
	m.OnConfigApplied(&configruntime.ConfigSnapshot{Version: configruntime.Version{Value: "version-2"}, Source: "file"})
	entries := m.audit.Entries()
	if len(entries) != 1 || entries[0].Action != "config:apply" || entries[0].Result != "success" || entries[0].Resource != "version-2" {
		t.Fatalf("post-commit audit entries=%#v", entries)
	}
}

func TestStreamKickRequiresCanonicalPathThroughRealMux(t *testing.T) {
	cfg := newTestConfig()
	cfg.API.Auth.Tokens = []config.APIAuthToken{
		{Name: "readonly", Token: "view-token", Role: "viewer"},
		{Name: "operations", Token: "ops-token", Role: "operator"},
	}
	server := core.NewServer(cfg)
	stream, err := server.StreamHub().GetOrCreate("live/camera/not-kick")
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.SetPublisher(&testPublisher{id: "publisher"}); err != nil {
		t.Fatal(err)
	}
	audit := NewAuditStore(8)
	mux := http.NewServeMux()
	registerRoutes(mux, server, audit)
	handler := buildSecurityHandler(mux, server, audit)

	nonCanonical := httptest.NewRequest(http.MethodPost, "/api/v1/streams/live/camera/not-kick", nil)
	nonCanonical.Header.Set("Authorization", "Bearer view-token")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, nonCanonical)
	if w.Code != http.StatusNotFound {
		t.Fatalf("noncanonical kick status=%d want=404 body=%s", w.Code, w.Body.String())
	}
	if stream.Publisher() == nil {
		t.Fatal("noncanonical kick path removed the publisher")
	}

	canonical := httptest.NewRequest(http.MethodPost, "/api/v1/streams/live/camera/not-kick/kick", nil)
	canonical.Header.Set("Authorization", "Bearer view-token")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, canonical)
	if w.Code != http.StatusForbidden || stream.Publisher() == nil {
		t.Fatalf("viewer canonical kick status=%d publisher=%v", w.Code, stream.Publisher())
	}

	canonical = httptest.NewRequest(http.MethodPost, "/api/v1/streams/live/camera/not-kick/kick", nil)
	canonical.Header.Set("Authorization", "Bearer ops-token")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, canonical)
	if w.Code != http.StatusOK || stream.Publisher() != nil {
		t.Fatalf("operator canonical kick status=%d publisher=%v body=%s", w.Code, stream.Publisher(), w.Body.String())
	}
	entries := audit.Entries()
	if len(entries) != 2 || entries[0].Action != "streams:kick" || entries[0].Result != "denied" || entries[1].Action != "streams:kick" || entries[1].Result != "success" {
		t.Fatalf("kick audit entries=%#v", entries)
	}
}

func TestGB28181MutationsUseExplicitPermissionsAndAuditThroughRealMux(t *testing.T) {
	cfg := newTestConfig()
	cfg.API.Auth.Tokens = []config.APIAuthToken{
		{Name: "readonly", Token: "view-token", Role: "viewer"},
		{Name: "operations", Token: "ops-token", Role: "operator"},
		{Name: "administrator", Token: "admin-token", Role: "admin"},
	}
	server := core.NewServer(cfg)
	mutations := make(map[string]int)
	for pattern := range map[string]struct{}{
		"DELETE /api/v1/gb28181/devices/":  {},
		"DELETE /api/v1/gb28181/sessions/": {},
		"DELETE /api/v1/gb28181/channels/": {},
		"POST /api/v1/gb28181/channels/":   {},
		"POST /api/relay/push":             {},
	} {
		pattern := pattern
		server.RegisterAPIHandler(pattern, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mutations[pattern]++
			w.WriteHeader(http.StatusNoContent)
		}))
	}
	audit := NewAuditStore(32)
	mux := http.NewServeMux()
	registerRoutes(mux, server, audit)
	handler := buildSecurityHandler(mux, server, audit)

	tests := []struct {
		name       string
		method     string
		path       string
		pattern    string
		permission string
		allowedFor string
	}{
		{name: "delete device", method: http.MethodDelete, path: "/api/v1/gb28181/devices/device-1", pattern: "DELETE /api/v1/gb28181/devices/", permission: "gb28181:delete", allowedFor: "admin-token"},
		{name: "delete session", method: http.MethodDelete, path: "/api/v1/gb28181/sessions/session-1", pattern: "DELETE /api/v1/gb28181/sessions/", permission: "gb28181:delete", allowedFor: "admin-token"},
		{name: "control channel", method: http.MethodPost, path: "/api/v1/gb28181/channels/channel-1/ptz", pattern: "POST /api/v1/gb28181/channels/", permission: "gb28181:control", allowedFor: "ops-token"},
		{name: "stop channel", method: http.MethodDelete, path: "/api/v1/gb28181/channels/channel-1/play", pattern: "DELETE /api/v1/gb28181/channels/", permission: "gb28181:control", allowedFor: "ops-token"},
		{name: "unclassified cross-module mutation", method: http.MethodPost, path: "/api/relay/push", pattern: "POST /api/relay/push", permission: "server:mutate", allowedFor: "admin-token"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := mutations[tt.pattern]
			denied := httptest.NewRequest(tt.method, tt.path, nil)
			denied.Header.Set("Authorization", "Bearer view-token")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, denied)
			if w.Code != http.StatusForbidden || mutations[tt.pattern] != before {
				t.Fatalf("viewer status=%d mutations=%d want=%d", w.Code, mutations[tt.pattern], before)
			}
			entry := audit.Entries()[len(audit.Entries())-1]
			if entry.Action != tt.permission || entry.Result != "denied" {
				t.Fatalf("denial audit=%#v", entry)
			}

			allowed := httptest.NewRequest(tt.method, tt.path, nil)
			allowed.Header.Set("Authorization", "Bearer "+tt.allowedFor)
			w = httptest.NewRecorder()
			handler.ServeHTTP(w, allowed)
			if w.Code != http.StatusNoContent || mutations[tt.pattern] != before+1 {
				t.Fatalf("allowed status=%d mutations=%d want=%d", w.Code, mutations[tt.pattern], before+1)
			}
			entry = audit.Entries()[len(audit.Entries())-1]
			if entry.Action != tt.permission || entry.Result != "success" {
				t.Fatalf("success audit=%#v", entry)
			}
		})
	}
}

func TestRateLimitedDestructiveRequestIsAudited(t *testing.T) {
	cfg := newTestConfig()
	cfg.API.Auth.BearerToken = "admin-token"
	cfg.Limits.RateLimit = config.RateLimitConfig{Enabled: true, Rate: 0.001, Burst: 1}
	m, _, addr := newTestModule(t, cfg)
	client := &http.Client{}

	request := func() int {
		req, err := http.NewRequest(http.MethodPost, addr+"/api/v1/server/config/refresh", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer admin-token")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	if status := request(); status != http.StatusServiceUnavailable {
		t.Fatalf("first status=%d want=503", status)
	}
	if status := request(); status != http.StatusTooManyRequests {
		t.Fatalf("second status=%d want=429", status)
	}
	entries := m.audit.Entries()
	if len(entries) != 2 || entries[1].Principal != "legacy-bearer" || entries[1].Action != "config:reload" || entries[1].Result != "failed" {
		t.Fatalf("rate-limit audit entries=%#v", entries)
	}
}

func TestDestructiveRoutesAuditSuccessAndFailureThroughRealMux(t *testing.T) {
	newHandler := func(server *core.Server, audit *AuditStore) http.Handler {
		t.Helper()
		mux := http.NewServeMux()
		registerRoutes(mux, server, audit)
		return buildSecurityHandler(mux, server, audit)
	}
	request := func(handler http.Handler, method, path string, body []byte) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer admin-token")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w
	}
	assertAudit := func(t *testing.T, audit *AuditStore, offset int, action, result string) {
		t.Helper()
		entries := audit.Entries()
		if len(entries) <= offset || entries[offset].Action != action || entries[offset].Result != result || entries[offset].RequestID == "" || entries[offset].Principal != "legacy-bearer" {
			t.Fatalf("audit[%d] action=%q result=%q entries=%#v", offset, action, result, entries)
		}
	}

	t.Run("stream delete", func(t *testing.T) {
		cfg := newTestConfig()
		cfg.API.Auth.BearerToken = "admin-token"
		server := core.NewServer(cfg)
		if _, err := server.StreamHub().GetOrCreate("live/delete"); err != nil {
			t.Fatal(err)
		}
		audit := NewAuditStore(8)
		handler := newHandler(server, audit)
		if w := request(handler, http.MethodDelete, "/api/v1/streams/live/delete", nil); w.Code != http.StatusOK {
			t.Fatalf("success status=%d body=%s", w.Code, w.Body.String())
		}
		if w := request(handler, http.MethodDelete, "/api/v1/streams/live/delete", nil); w.Code != http.StatusNotFound {
			t.Fatalf("failure status=%d body=%s", w.Code, w.Body.String())
		}
		assertAudit(t, audit, 0, "streams:delete", "success")
		assertAudit(t, audit, 1, "streams:delete", "failed")
	})

	t.Run("recording delete", func(t *testing.T) {
		cfg := newTestConfig()
		cfg.API.Auth.BearerToken = "admin-token"
		server := core.NewServer(cfg)
		server.RegisterModule(&recordingProviderStub{items: []record.RecordingInfo{{ID: "live/cam.flv", State: record.RecordingCompleted}}})
		audit := NewAuditStore(8)
		handler := newHandler(server, audit)
		if w := request(handler, http.MethodDelete, "/api/v1/recordings/live/cam.flv", nil); w.Code != http.StatusOK {
			t.Fatalf("success status=%d body=%s", w.Code, w.Body.String())
		}
		if w := request(handler, http.MethodDelete, "/api/v1/recordings/missing.flv", nil); w.Code != http.StatusNotFound {
			t.Fatalf("failure status=%d body=%s", w.Code, w.Body.String())
		}
		assertAudit(t, audit, 0, "recordings:delete", "success")
		assertAudit(t, audit, 1, "recordings:delete", "failed")
	})

	t.Run("SIP call control", func(t *testing.T) {
		cfg := newTestConfig()
		cfg.API.Auth.BearerToken = "admin-token"
		server := core.NewServer(cfg)
		server.RegisterModule(&sipGatewayStub{calls: map[string]sipgateway.CallSnapshot{"call-1": {CallID: "call-1"}}})
		audit := NewAuditStore(8)
		handler := newHandler(server, audit)
		if w := request(handler, http.MethodPost, "/api/v1/sipgateway/calls", []byte(`{"target_uri":"1001","stream_key":"live/audio"}`)); w.Code != http.StatusCreated {
			t.Fatalf("dial status=%d body=%s", w.Code, w.Body.String())
		}
		if w := request(handler, http.MethodPost, "/api/v1/sipgateway/calls", []byte(`{"target_uri":`)); w.Code != http.StatusBadRequest {
			t.Fatalf("failed dial status=%d body=%s", w.Code, w.Body.String())
		}
		if w := request(handler, http.MethodDelete, "/api/v1/sipgateway/calls/call-1", nil); w.Code != http.StatusOK {
			t.Fatalf("hangup status=%d body=%s", w.Code, w.Body.String())
		}
		assertAudit(t, audit, 0, "sip:calls", "success")
		assertAudit(t, audit, 1, "sip:calls", "failed")
		assertAudit(t, audit, 2, "sip:calls", "success")
	})

	t.Run("config refresh", func(t *testing.T) {
		cfg := newTestConfig()
		cfg.API.Auth.BearerToken = "admin-token"
		server := core.NewServer(cfg)
		manager, err := configruntime.NewManager(configruntime.Options{Source: securityConfigSource{}, Initial: cfg})
		if err != nil {
			t.Fatal(err)
		}
		server.SetConfigManager(manager)
		if err := manager.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		audit := NewAuditStore(8)
		handler := newHandler(server, audit)
		if w := request(handler, http.MethodPost, "/api/v1/server/config/refresh", nil); w.Code != http.StatusAccepted {
			t.Fatalf("success status=%d body=%s", w.Code, w.Body.String())
		}
		if err := manager.Close(); err != nil {
			t.Fatal(err)
		}
		if w := request(handler, http.MethodPost, "/api/v1/server/config/refresh", nil); w.Code != http.StatusServiceUnavailable {
			t.Fatalf("failure status=%d body=%s", w.Code, w.Body.String())
		}
		assertAudit(t, audit, 0, "config:reload", "success")
		assertAudit(t, audit, 1, "config:reload", "failed")
	})
}
