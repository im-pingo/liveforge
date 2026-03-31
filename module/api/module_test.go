package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
)

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

func TestSessionTokenValidation(t *testing.T) {
	cfg := config.ConsoleConfig{
		Username: "admin",
		Password: "pass123",
	}

	token := generateSessionToken(cfg)
	if token == "" {
		t.Fatal("expected non-empty token")
	}

	// Create a request with the token
	req, _ := http.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: "lf_session", Value: token})
	if !validateSession(req, cfg) {
		t.Error("valid token should validate")
	}

	// Invalid token
	req2, _ := http.NewRequest("GET", "/", nil)
	req2.AddCookie(&http.Cookie{Name: "lf_session", Value: "invalid.token"})
	if validateSession(req2, cfg) {
		t.Error("invalid token should not validate")
	}

	// No cookie
	req3, _ := http.NewRequest("GET", "/", nil)
	if validateSession(req3, cfg) {
		t.Error("no cookie should not validate")
	}

	// Malformed cookie (no dot separator)
	req4, _ := http.NewRequest("GET", "/", nil)
	req4.AddCookie(&http.Cookie{Name: "lf_session", Value: "nodot"})
	if validateSession(req4, cfg) {
		t.Error("malformed token should not validate")
	}
}
