package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/module/cluster"
)

func TestRegisteredManagementHandlersRequireAuthorization(t *testing.T) {
	for _, path := range []string{"/api/relay/push", "/relay/push", "/console/relay/push", "/console/static/relay/push", "/api/v1/streams/relay/push", "/api/v1/server/config/relay/push", "/api/v1/gb28181/channels/relay/push", "/api/v1/server/health", "/console/login", "/console/logout"} {
		t.Run(path, func(t *testing.T) {
			cfg := config.Defaults()
			cfg.API.Console.Username = ""
			cfg.API.Auth.Tokens = []config.APIAuthToken{
				{Name: "viewer", Role: "viewer", Token: "viewer-token"},
				{Name: "operator", Role: "operator", Token: "operator-token"},
				{Name: "admin", Role: "admin", Token: "admin-token"},
			}
			server := core.NewServer(cfg)
			calls := 0
			server.RegisterAPIHandler("POST "+path, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				w.WriteHeader(http.StatusNoContent)
			}))
			mux := http.NewServeMux()
			registerRoutes(mux, server, NewAuditStore(16))
			audit := NewAuditStore(16)
			handler := buildSecurityHandler(mux, server, audit)
			for _, tc := range []struct {
				token string
				want  int
			}{{"", 401}, {"viewer-token", 403}, {"operator-token", 403}, {"admin-token", 204}} {
				r := httptest.NewRequest(http.MethodPost, path, nil)
				if tc.token != "" {
					r.Header.Set("Authorization", "Bearer "+tc.token)
				}
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, r)
				if w.Code != tc.want {
					t.Errorf("token %q: status=%d want=%d", tc.token, w.Code, tc.want)
				}
			}
			if calls != 1 {
				t.Errorf("protected handler called %d times, want one admin request", calls)
			}
			entries := audit.Entries()
			if len(entries) != 4 {
				t.Fatalf("audit entries=%d, want all four requests", len(entries))
			}
			for i, result := range []string{"unauthorized", "denied", "denied", "success"} {
				if entries[i].Action != "server:mutate" || entries[i].Result != result {
					t.Errorf("audit[%d]=%+v, want server:mutate %s", i, entries[i], result)
				}
			}
		})
	}
}

func TestClusterRegisteredPermissionsDoNotDependOnSignalingPath(t *testing.T) {
	for _, protocol := range []string{"rtp", "gb"} {
		t.Run(protocol, func(t *testing.T) {
			cfg := config.Defaults()
			cfg.API.Auth.Tokens = []config.APIAuthToken{{Name: "viewer", Role: "viewer", Token: "viewer-token"}}
			server := core.NewServer(cfg)
			path := "/api/v1/streams/relay"
			if protocol == "rtp" {
				transport := cluster.NewRTPTransport(config.ClusterRTPConfig{SignalingPath: path}, server)
				t.Cleanup(func() { transport.Close() })
			} else {
				transport := cluster.NewGBTransport(config.ClusterGBConfig{SignalingPath: path}, server)
				t.Cleanup(func() { transport.Close() })
			}
			mux := http.NewServeMux()
			registerRoutes(mux, server, nil)
			audit := NewAuditStore(16)
			handler := buildSecurityHandler(mux, server, audit)
			for _, action := range []string{"push", "pull"} {
				registered, ok := server.APIHandlers()["POST "+path+"/"+action].(core.APIPermissionHandler)
				if !ok || registered.APIPermission() != "server:mutate" {
					t.Fatalf("%s registration does not declare server:mutate", action)
				}
				r := httptest.NewRequest(http.MethodPost, path+"/"+action, nil)
				r.Header.Set("Authorization", "Bearer viewer-token")
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, r)
				if w.Code != http.StatusForbidden {
					t.Errorf("%s status=%d, want403 before media setup", action, w.Code)
				}
			}
			if entries := audit.Entries(); len(entries) != 2 || entries[0].Action != "server:mutate" || entries[1].Action != "server:mutate" {
				t.Errorf("cluster permission audit=%+v", entries)
			}
		})
	}
}

func TestRegisteredPermissionMetadataOverridesPathAndMethod(t *testing.T) {
	for _, tc := range []struct {
		permission string
		viewer     int
		operator   int
	}{
		{"gb28181:read", http.StatusNoContent, http.StatusNoContent},
		{"gb28181:control", http.StatusForbidden, http.StatusNoContent},
		{"gb28181:delete", http.StatusForbidden, http.StatusForbidden},
		{"server:mutate", http.StatusForbidden, http.StatusForbidden},
	} {
		t.Run(tc.permission, func(t *testing.T) {
			cfg := config.Defaults()
			cfg.API.Auth.Tokens = []config.APIAuthToken{
				{Name: "viewer", Role: "viewer", Token: "viewer-token"},
				{Name: "operator", Role: "operator", Token: "operator-token"},
			}
			server := core.NewServer(cfg)
			server.RegisterAPIHandler("GET /api/v1/streams/plugin", core.WithAPIPermission(tc.permission, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})))
			mux := http.NewServeMux()
			registerRoutes(mux, server, nil)
			handler := buildSecurityHandler(mux, server, NewAuditStore(8))
			for token, want := range map[string]int{"viewer-token": tc.viewer, "operator-token": tc.operator} {
				r := httptest.NewRequest(http.MethodGet, "/api/v1/streams/plugin", nil)
				r.Header.Set("Authorization", "Bearer "+token)
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, r)
				if w.Code != want {
					t.Errorf("token=%s status=%d want=%d", token, w.Code, want)
				}
			}
		})
	}
}

func TestRegisteredSubtreeKeepsMoreSpecificBuiltinPermissions(t *testing.T) {
	cfg := config.Defaults()
	cfg.API.Auth.Tokens = []config.APIAuthToken{{Name: "viewer", Role: "viewer", Token: "viewer-token"}}
	server := core.NewServer(cfg)
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		server.RegisterAPIHandler(method+" /api/v1/server/", core.WithAPIPermission("server:mutate", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})))
	}
	server.RegisterAPIHandler("GET /debug/plugin", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	mux := http.NewServeMux()
	registerRoutes(mux, server, nil)
	handler := buildSecurityHandler(mux, server, NewAuditStore(8))
	for _, tc := range []struct {
		method, path, token string
		want                int
	}{
		{http.MethodPost, "/api/v1/server/config/validate", "viewer-token", http.StatusOK},
		{http.MethodGet, "/api/v1/server/health", "", http.StatusOK},
		{http.MethodPost, "/api/v1/server/plugin", "viewer-token", http.StatusForbidden},
		{http.MethodGet, "/debug/plugin", "viewer-token", http.StatusForbidden},
	} {
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader("server:\n  name: review\n"))
		if tc.token != "" {
			r.Header.Set("Authorization", "Bearer "+tc.token)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Errorf("%s %s status=%d want=%d", tc.method, tc.path, w.Code, tc.want)
		}
	}
}

func TestRateLimitedRegisteredRequestsUseMatchedPermissions(t *testing.T) {
	for _, tc := range []struct {
		name, pattern, path, declared, action string
		wantAudit                             int
		wantFirst                             int
	}{
		{"unknown custom mutation", "POST /api/v1/streams/relay/push", "/api/v1/streams/relay/push", "", "server:mutate", 2, http.StatusNoContent},
		{"declared custom control", "POST /api/v1/streams/relay/push", "/api/v1/streams/relay/push", "gb28181:control", "gb28181:control", 2, http.StatusNoContent},
		{"builtin read under custom subtree", "POST /api/v1/server/", "/api/v1/server/config/validate", "server:mutate", "", 0, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := newTestConfig()
			cfg.API.Listen = "127.0.0.1:0"
			cfg.API.Auth.BearerToken = "admin-token"
			cfg.Limits.RateLimit = config.RateLimitConfig{Enabled: true, Rate: 0.001, Burst: 1}
			server := core.NewServer(cfg)
			var endpoint http.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})
			if tc.declared != "" {
				endpoint = core.WithAPIPermission(tc.declared, endpoint)
			}
			server.RegisterAPIHandler(tc.pattern, endpoint)
			module := NewModule()
			server.RegisterModule(module)
			if err := server.Init(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(server.Shutdown)
			for _, want := range []int{tc.wantFirst, http.StatusTooManyRequests} {
				r := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader("server:\n  name: review\n"))
				r.Header.Set("Authorization", "Bearer admin-token")
				w := httptest.NewRecorder()
				module.httpSrv.Handler.ServeHTTP(w, r)
				if w.Code != want {
					t.Fatalf("status=%d want=%d", w.Code, want)
				}
			}
			entries := module.audit.Entries()
			if len(entries) != tc.wantAudit {
				t.Fatalf("audit entries=%d want=%d: %+v", len(entries), tc.wantAudit, entries)
			}
			if tc.wantAudit > 0 && (entries[0].Action != tc.action || entries[0].Result != "success" || entries[1].Action != tc.action || entries[1].Result != "failed") {
				t.Errorf("normal and throttled requests use different audit permissions: %+v", entries)
			}
		})
	}
}

func TestConsoleLogoutClearsSessionCookie(t *testing.T) {
	cfg := config.Defaults()
	cfg.API.Console.Username = "review-user"
	cfg.API.Console.Password = "test-password"
	server := core.NewServer(cfg)
	handler := buildSecurityHandler(http.NotFoundHandler(), server, NewAuditStore(4))
	r := httptest.NewRequest(http.MethodPost, "/console/logout", nil)
	r.AddCookie(&http.Cookie{Name: "lf_session", Value: generateSessionToken(cfg.API.Console)}) // #nosec G124 -- Request cookies contain only name/value; response security attributes are asserted below.
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/console/login" {
		t.Fatalf("logout status=%d location=%q", w.Code, w.Header().Get("Location"))
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != "lf_session" || cookies[0].Value != "" || cookies[0].MaxAge != -1 || !cookies[0].HttpOnly || cookies[0].Path != "/" || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("logout cookie=%#v", cookies)
	}
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/console/logout", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET logout status=%d want=405", w.Code)
	}
}
