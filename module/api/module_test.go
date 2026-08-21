package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
)

type blockingConfigModule struct {
	applyStarted chan struct{}
	releaseApply chan struct{}
}

func (m *blockingConfigModule) Name() string                                   { return "blocking-config" }
func (m *blockingConfigModule) Init(*core.Server) error                        { return nil }
func (m *blockingConfigModule) Hooks() []core.HookRegistration                 { return nil }
func (m *blockingConfigModule) Close() error                                   { return nil }
func (m *blockingConfigModule) ValidateConfigChange(_, _ *config.Config) error { return nil }
func (m *blockingConfigModule) ApplyConfigChange(_, _ *config.Config) {
	close(m.applyStarted)
	<-m.releaseApply
}

// newTestModule creates a fully initialized API module on a random port.
func newTestModule(t *testing.T, cfg *config.Config) (*Module, *core.Server, string) {
	t.Helper()
	if cfg == nil {
		cfg = newTestConfig()
	}
	cfg.API.Listen = "127.0.0.1:0"

	srv := core.NewServer(cfg)
	m := NewModule()
	srv.RegisterModule(m)
	if err := srv.Init(); err != nil {
		t.Fatalf("server init: %v", err)
	}
	t.Cleanup(srv.Shutdown)

	addr := "http://" + m.Addr().String()
	return m, srv, addr
}

func TestModuleName(t *testing.T) {
	m := NewModule()
	if m.Name() != "api" {
		t.Errorf("expected 'api', got %q", m.Name())
	}
}

func TestModuleHooks(t *testing.T) {
	m := NewModule()
	if hooks := m.Hooks(); hooks != nil {
		t.Error("expected nil hooks")
	}
}

func TestModuleInitAndClose(t *testing.T) {
	_, _, addr := newTestModule(t, nil)

	// Health endpoint should respond
	resp, err := http.Get(addr + "/api/v1/server/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}

func TestRoutes(t *testing.T) {
	_, _, addr := newTestModule(t, nil)
	client := &http.Client{Timeout: 2 * time.Second}

	tests := []struct {
		method string
		path   string
		status int
	}{
		{"GET", "/api/v1/streams", http.StatusOK},
		{"GET", "/api/v1/server/info", http.StatusOK},
		{"GET", "/api/v1/server/stats", http.StatusOK},
		{"GET", "/api/v1/server/health", http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			req, _ := http.NewRequest(tt.method, addr+tt.path, nil)
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tt.status {
				t.Errorf("expected %d, got %d", tt.status, resp.StatusCode)
			}
		})
	}
}

func TestServerInfoEndpoint(t *testing.T) {
	cfg := newTestConfig()
	cfg.HTTP.Enabled = true
	cfg.HTTP.Listen = ":8080"
	cfg.RTMP.Enabled = true
	cfg.RTMP.Listen = ":1935"

	_, _, addr := newTestModule(t, cfg)

	resp, err := http.Get(addr + "/api/v1/server/info")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	data := decodeAPIData(t, body)

	var info ServerInfo
	if err := json.Unmarshal(data, &info); err != nil {
		t.Fatal(err)
	}
	if info.Version == "" {
		t.Error("expected version")
	}
	if info.Endpoints["http"] != ":8080" {
		t.Errorf("expected http endpoint :8080, got %q", info.Endpoints["http"])
	}
	if info.Endpoints["rtmp"] != ":1935" {
		t.Errorf("expected rtmp endpoint :1935, got %q", info.Endpoints["rtmp"])
	}
}

func TestBearerTokenAuth(t *testing.T) {
	cfg := newTestConfig()
	cfg.API.Auth.Enabled = true
	cfg.API.Auth.BearerToken = "secret-token-123"

	_, _, addr := newTestModule(t, cfg)
	client := &http.Client{Timeout: 2 * time.Second}

	// Without token — should get 401
	resp, err := client.Get(addr + "/api/v1/streams")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no token: expected 401, got %d", resp.StatusCode)
	}

	// With wrong token — should get 401
	req, _ := http.NewRequest("GET", addr+"/api/v1/streams", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong token: expected 401, got %d", resp.StatusCode)
	}

	// With correct token — should get 200
	req, _ = http.NewRequest("GET", addr+"/api/v1/streams", nil)
	req.Header.Set("Authorization", "Bearer secret-token-123")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("correct token: expected 200, got %d", resp.StatusCode)
	}
}

func TestEnabledAPIAuthFailsClosedWithoutBearerToken(t *testing.T) {
	cfg := newTestConfig()
	cfg.API.Auth.Enabled = true
	cfg.API.Console.Username = "admin"
	cfg.API.Console.Password = "pass123"
	_, _, addr := newTestModule(t, cfg)

	resp, err := http.Get(addr + "/api/v1/streams")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestDisabledAPIAuthIgnoresRetainedBearerForOrdinaryRoutes(t *testing.T) {
	cfg := newTestConfig()
	cfg.API.Auth.Enabled = false
	cfg.API.Auth.BearerToken = "retained-token"
	cfg.API.Console.Username = "admin"
	cfg.API.Console.Password = "pass123"
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/streams", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("/api/v1/config", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	handler := buildAuthHandler(mux, cfg.API)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/streams", nil))
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("ordinary API status = %d, want 204", recorder.Code)
	}

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/config", nil))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated settings status = %d, want 401", recorder.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/config", nil)
	req.Header.Set("Authorization", "Bearer retained-token")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("bearer-authenticated settings status = %d, want 204", recorder.Code)
	}
}

func TestAPIAuthReadsRuntimeConfig(t *testing.T) {
	cfg := newTestConfig()
	cfg.API.Auth.Enabled = true
	cfg.API.Auth.BearerToken = "old-token"
	_, server, addr := newTestModule(t, cfg)

	next := *cfg
	next.API.Auth.BearerToken = "new-token"
	if err := server.ApplyConfig(&next); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 2 * time.Second}
	for token, want := range map[string]int{"old-token": http.StatusUnauthorized, "new-token": http.StatusOK} {
		req, _ := http.NewRequest(http.MethodGet, addr+"/api/v1/streams", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("token %q status = %d, want %d", token, resp.StatusCode, want)
		}
	}
}

func TestConfigEndpointRequiresAuthentication(t *testing.T) {
	cfg := newTestConfig()
	cfg.API.Auth.Enabled = false
	cfg.API.Console.Username = "admin"
	cfg.API.Console.Password = "pass123"
	_, _, addr := newTestModule(t, cfg)

	resp, err := http.Get(addr + "/api/v1/config")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestPprofDisabledByDefault(t *testing.T) {
	cfg := newTestConfig()
	cfg.API.Auth.Enabled = false
	_, _, addr := newTestModule(t, cfg)
	resp, err := http.Get(addr + "/debug/pprof/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestConsoleSessionAuth(t *testing.T) {
	cfg := newTestConfig()
	cfg.API.Console.Username = "admin"
	cfg.API.Console.Password = "pass123"

	_, _, addr := newTestModule(t, cfg)
	client := &http.Client{
		Timeout: 2 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse // don't follow redirects
		},
	}

	// Login page (GET) should return 200
	resp, err := client.Get(addr + "/console/login")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("login page: expected 200, got %d", resp.StatusCode)
	}

	// Login with wrong credentials — should get 401
	form := url.Values{"username": {"admin"}, "password": {"wrong"}}
	resp, err = client.PostForm(addr+"/console/login", form)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong password: expected 401, got %d", resp.StatusCode)
	}

	// Login with correct credentials — should get redirect (303)
	form = url.Values{"username": {"admin"}, "password": {"pass123"}}
	resp, err = client.PostForm(addr+"/console/login", form)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("correct login: expected 303, got %d", resp.StatusCode)
	}

	// Check that session cookie was set
	cookies := resp.Cookies()
	var sessionCookie *http.Cookie
	for _, c := range cookies {
		if c.Name == "lf_session" {
			sessionCookie = c
			break
		}
	}
	if sessionCookie == nil {
		t.Fatal("expected lf_session cookie after login")
	}

	// Access console with session cookie — should get 200
	req, _ := http.NewRequest("GET", addr+"/console", nil)
	req.AddCookie(sessionCookie)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("console with session: expected 200, got %d", resp.StatusCode)
	}

	// Console without session — should redirect to login
	resp, err = client.Get(addr + "/console")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Errorf("console without session: expected 302, got %d", resp.StatusCode)
	}

	// API with session cookie should also work (when bearer token is configured)
	cfg2 := newTestConfig()
	cfg2.API.Auth.Enabled = true
	cfg2.API.Auth.BearerToken = "tok"
	cfg2.API.Console.Username = "admin"
	cfg2.API.Console.Password = "pass123"
	_, _, addr2 := newTestModule(t, cfg2)

	// Login first
	resp, err = client.PostForm(addr2+"/console/login", url.Values{"username": {"admin"}, "password": {"pass123"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	for _, c := range resp.Cookies() {
		if c.Name == "lf_session" {
			sessionCookie = c
		}
	}

	// Access API with session cookie instead of bearer token
	req, _ = http.NewRequest("GET", addr2+"/api/v1/streams", nil)
	req.AddCookie(sessionCookie)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("API with session: expected 200, got %d", resp.StatusCode)
	}
}

func TestLoginMethodNotAllowed(t *testing.T) {
	cfg := newTestConfig()
	cfg.API.Console.Username = "admin"
	cfg.API.Console.Password = "pass123"

	_, _, addr := newTestModule(t, cfg)

	req, _ := http.NewRequest("PUT", addr+"/console/login", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", resp.StatusCode)
	}
}

func TestWriteError(t *testing.T) {
	_, _, addr := newTestModule(t, nil)

	// Stream not found
	resp, err := http.Get(addr + "/api/v1/streams/nonexist")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	var envelope struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	json.Unmarshal(body, &envelope)
	if envelope.Code != http.StatusNotFound {
		t.Errorf("expected code 404, got %d", envelope.Code)
	}
	if !strings.Contains(envelope.Message, "not found") {
		t.Errorf("expected 'not found' in message, got %q", envelope.Message)
	}
}

func TestStreamDetailNotFound(t *testing.T) {
	_, _, addr := newTestModule(t, nil)

	resp, err := http.Get(addr + "/api/v1/streams/live/missing")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestStreamDeleteNotFound(t *testing.T) {
	_, _, addr := newTestModule(t, nil)

	req, _ := http.NewRequest("DELETE", addr+"/api/v1/streams/live/missing", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestKickNotFound(t *testing.T) {
	_, _, addr := newTestModule(t, nil)

	resp, err := http.Post(addr+"/api/v1/streams/live/missing/kick", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestKickNoPublisher(t *testing.T) {
	_, srv, addr := newTestModule(t, nil)

	// Create stream without publisher
	srv.StreamHub().GetOrCreate("live/nopub")

	resp, err := http.Post(addr+"/api/v1/streams/live/nopub/kick", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("expected 409, got %d", resp.StatusCode)
	}
}

func TestSessionManagerTokenValidation(t *testing.T) {
	sessions, err := newSessionManager()
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.ConsoleConfig{Username: "admin", Password: "pass123"}
	token, ok := sessions.generateTokenFor(sessions.generationSnapshot(), cfg)
	if !ok {
		t.Fatal("generate session token")
	}
	if token == "" {
		t.Fatal("expected non-empty token")
	}

	// Create a request with the token
	req, _ := http.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: "lf_session", Value: token})
	if !sessions.validate(req, cfg) {
		t.Error("valid token should validate")
	}

	// Invalid token
	req2, _ := http.NewRequest("GET", "/", nil)
	req2.AddCookie(&http.Cookie{Name: "lf_session", Value: "invalid.token"})
	if sessions.validate(req2, cfg) {
		t.Error("invalid token should not validate")
	}

	// No cookie
	req3, _ := http.NewRequest("GET", "/", nil)
	if sessions.validate(req3, cfg) {
		t.Error("no cookie should not validate")
	}

	// Malformed cookie (no dot separator)
	req4, _ := http.NewRequest("GET", "/", nil)
	req4.AddCookie(&http.Cookie{Name: "lf_session", Value: "nodot"})
	if sessions.validate(req4, cfg) {
		t.Error("malformed token should not validate")
	}
}

func TestSessionManagerRevokesPreviousGeneration(t *testing.T) {
	sessions, err := newSessionManager()
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.ConsoleConfig{Username: "admin", Password: "pass123"}
	token, ok := sessions.generateTokenFor(sessions.generationSnapshot(), cfg)
	if !ok {
		t.Fatal("generate session token")
	}
	req := httptest.NewRequest(http.MethodGet, "/console", nil)
	req.AddCookie(&http.Cookie{Name: "lf_session", Value: token})
	if !sessions.validate(req, cfg) {
		t.Fatal("new session token did not validate")
	}

	sessions.revokeAll()
	if sessions.validate(req, cfg) {
		t.Fatal("session from previous generation still validated")
	}
}

func TestSessionManagerRejectsIssueAfterGenerationChanges(t *testing.T) {
	sessions, err := newSessionManager()
	if err != nil {
		t.Fatal(err)
	}
	generation := sessions.generationSnapshot()
	sessions.revokeAll()
	if token, ok := sessions.generateTokenFor(generation, config.ConsoleConfig{Username: "admin", Password: "pass123"}); ok || token != "" {
		t.Fatal("session token was issued for a revoked credential generation")
	}
}

func TestLoginRejectsCredentialsReadAcrossGenerationChange(t *testing.T) {
	sessions, err := newSessionManager()
	if err != nil {
		t.Fatal(err)
	}
	cfg := newTestConfig().API
	cfg.Console.Username = "admin"
	cfg.Console.Password = "old-password"
	handler := buildAuthHandlerWithConfig(http.NewServeMux(), func() config.APIConfig {
		sessions.revokeAll()
		return cfg
	}, sessions)
	form := url.Values{"username": {"admin"}, "password": {"old-password"}}
	req := httptest.NewRequest(http.MethodPost, "/console/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("login status = %d, want 401 after credential generation changed", recorder.Code)
	}
	if cookies := recorder.Result().Cookies(); len(cookies) != 0 {
		t.Fatalf("stale credential login set cookies: %+v", cookies)
	}
}

func TestRuntimeConsoleCredentialChangeRevokesSessions(t *testing.T) {
	cfg := newTestConfig()
	cfg.API.Console.Username = "admin"
	cfg.API.Console.Password = "old-password"
	cfg.API.Auth.Enabled = true
	_, server, addr := newTestModule(t, cfg)
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	loginResp, err := client.PostForm(addr+"/console/login", url.Values{
		"username": {"admin"},
		"password": {"old-password"},
	})
	if err != nil {
		t.Fatal(err)
	}
	loginResp.Body.Close()
	var sessionCookie *http.Cookie
	for _, cookie := range loginResp.Cookies() {
		if cookie.Name == "lf_session" {
			sessionCookie = cookie
			break
		}
	}
	if sessionCookie == nil {
		t.Fatal("login response did not set lf_session")
	}

	requestConfig := func() int {
		req := httptest.NewRequest(http.MethodGet, addr+"/api/v1/streams", nil)
		req.RequestURI = ""
		req.AddCookie(sessionCookie)
		resp, requestErr := http.DefaultClient.Do(req)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := requestConfig(); got != http.StatusOK {
		t.Fatalf("session status before credential change = %d, want 200", got)
	}

	next := *server.Config()
	next.API.Console.Password = "new-password"
	if err := server.ApplyConfig(&next); err != nil {
		t.Fatal(err)
	}
	if got := requestConfig(); got != http.StatusUnauthorized {
		t.Fatalf("session status after credential change = %d, want 401", got)
	}
}

func TestCredentialRotationInvalidatesOldSessionAtConfigPublication(t *testing.T) {
	cfg := newTestConfig()
	cfg.API.Listen = "127.0.0.1:0"
	cfg.API.Auth.Enabled = true
	cfg.API.Console.Username = "admin"
	cfg.API.Console.Password = "old-password"
	server := core.NewServer(cfg)
	blocker := &blockingConfigModule{
		applyStarted: make(chan struct{}),
		releaseApply: make(chan struct{}),
	}
	apiModule := NewModule()
	server.RegisterModule(blocker)
	server.RegisterModule(apiModule)
	if err := server.Init(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Shutdown)
	released := false
	t.Cleanup(func() {
		if !released {
			close(blocker.releaseApply)
		}
	})

	addr := "http://" + apiModule.Addr().String()
	client := &http.Client{
		Timeout:       2 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	login := func(password string) (*http.Cookie, int) {
		resp, err := client.PostForm(addr+"/console/login", url.Values{
			"username": {"admin"},
			"password": {password},
		})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		for _, cookie := range resp.Cookies() {
			if cookie.Name == "lf_session" {
				return cookie, resp.StatusCode
			}
		}
		return nil, resp.StatusCode
	}
	requestWithSession := func(cookie *http.Cookie) int {
		req, err := http.NewRequest(http.MethodGet, addr+"/api/v1/streams", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.AddCookie(cookie)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	oldSession, status := login("old-password")
	if status != http.StatusSeeOther || oldSession == nil {
		t.Fatalf("old credential login status/cookie = %d/%v, want 303/session", status, oldSession)
	}

	next := *server.Config()
	next.API.Console.Password = "new-password"
	applyDone := make(chan error, 1)
	go func() { applyDone <- server.ApplyConfig(&next) }()
	select {
	case <-blocker.applyStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("config apply did not block after publication")
	}
	if got := server.Config().API.Console.Password; got != "new-password" {
		t.Fatalf("published password = %q, want new-password", got)
	}
	if got := requestWithSession(oldSession); got != http.StatusUnauthorized {
		t.Fatalf("old session status during blocked apply = %d, want 401", got)
	}

	newSession, status := login("new-password")
	if status != http.StatusSeeOther || newSession == nil {
		t.Fatalf("new credential login status/cookie = %d/%v, want 303/session", status, newSession)
	}
	close(blocker.releaseApply)
	released = true
	if err := <-applyDone; err != nil {
		t.Fatal(err)
	}
	if got := requestWithSession(newSession); got != http.StatusOK {
		t.Fatalf("new session status after apply = %d, want 200", got)
	}
}
