package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"
)

type configTestServer struct {
	addr        string
	server      *core.Server
	manager     *config.Manager
	bearerToken string
	password    string
}

func newConfigTestServer(t *testing.T) configTestServer {
	t.Helper()
	const (
		bearerToken = "test-bearer-secret"
		password    = "current-password"
	)
	passwordHash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	cfg := newTestConfig()
	cfg.Server.Name = "Config Test"
	cfg.Server.LogLevel = "info"
	cfg.API.Listen = "127.0.0.1:0"
	cfg.API.Auth.Enabled = true
	cfg.API.Auth.BearerToken = bearerToken
	cfg.API.Console = config.ConsoleConfig{
		Username:     "admin",
		Password:     "legacy-plain-secret",
		PasswordHash: string(passwordHash),
	}
	cfg.Auth.Enabled = true
	cfg.Auth.Publish = config.AuthRuleConfig{
		Mode:  "token+callback",
		Stage: "pre_session",
		Token: config.TokenConfig{Secret: "publish-token-secret", Algorithm: "HS256"},
		Callback: config.CallbackConfig{
			URL: "https://auth.example/publish?key=publish-callback-secret", Timeout: 3 * time.Second,
		},
	}
	cfg.Auth.Subscribe = config.AuthRuleConfig{
		Mode:  "token",
		Stage: "post_connect",
		Token: config.TokenConfig{Secret: "subscribe-token-secret", Algorithm: "HS256"},
	}

	baseData, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	basePath := filepath.Join(t.TempDir(), "liveforge.yaml")
	if err := os.WriteFile(basePath, baseData, 0o600); err != nil {
		t.Fatal(err)
	}
	source := config.NewFileSource(basePath, config.RuntimeOverridePath(basePath))
	manager := config.NewManager(source, time.Hour, nil)
	if _, err := manager.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	server := core.NewServer(manager.Current().Effective)
	server.SetConfigUpdater(manager)
	manager.SetApply(func(_ context.Context, _, next *config.Config, _ config.ChangeSet) error {
		return server.ApplyConfig(next)
	})
	apiModule := NewModule()
	server.RegisterModule(apiModule)
	if err := server.Init(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Shutdown)

	return configTestServer{
		addr:        "http://" + apiModule.Addr().String(),
		server:      server,
		manager:     manager,
		bearerToken: bearerToken,
		password:    password,
	}
}

func (s configTestServer) request(t *testing.T, method, path string, body io.Reader) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, s.addr+path, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+s.bearerToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func (s configTestServer) login(t *testing.T, password string) *http.Cookie {
	t.Helper()
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.PostForm(s.addr+"/console/login", url.Values{
		"username": {"admin"},
		"password": {password},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("login status = %d, want 303: %s", resp.StatusCode, body)
	}
	for _, cookie := range resp.Cookies() {
		if cookie.Name == "lf_session" {
			return cookie
		}
	}
	t.Fatal("login response did not set lf_session")
	return nil
}

func TestGetConfigReturnsETagAndRedactedRuntimeView(t *testing.T) {
	testServer := newConfigTestServer(t)
	resp := testServer.request(t, http.MethodGet, "/api/v1/config", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}

	snapshot := testServer.manager.Current()
	if got, want := resp.Header.Get("ETag"), `"`+snapshot.Revision+`"`; got != want {
		t.Errorf("ETag = %q, want %q", got, want)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	for _, value := range []string{"Cookie", "Authorization"} {
		if !strings.Contains(resp.Header.Get("Vary"), value) {
			t.Errorf("Vary = %q, want %s", resp.Header.Get("Vary"), value)
		}
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{
		testServer.bearerToken, "legacy-plain-secret", snapshot.Effective.API.Console.PasswordHash, "$2",
		"publish-token-secret", "subscribe-token-secret", "publish-callback-secret", "https://auth.example",
	} {
		if strings.Contains(string(body), secret) {
			t.Errorf("config response leaked secret %q", secret)
		}
	}

	data := decodeAPIData(t, body)
	var view configView
	if err := json.Unmarshal(data, &view); err != nil {
		t.Fatal(err)
	}
	if view.Revision != snapshot.Revision || view.Source.Name != snapshot.Source {
		t.Errorf("revision/source = %q/%q, want %q/%q", view.Revision, view.Source.Name, snapshot.Revision, snapshot.Source)
	}
	if view.Desired.Server.Name != "Config Test" || view.Desired.Server.LogLevel != "info" ||
		view.Effective.Server != view.Desired.Server {
		t.Errorf("server desired/effective = %+v / %+v", view.Desired.Server, view.Effective.Server)
	}
	auth := view.Effective.Auth
	if !auth.Enabled || auth.Publish.Mode != "token+callback" || auth.Publish.Stage != "pre_session" ||
		auth.Publish.Token.Algorithm != "HS256" || !auth.Publish.Token.SecretConfigured ||
		!auth.Publish.Callback.URLConfigured || auth.Publish.Callback.Timeout != "3s" ||
		auth.Subscribe.Mode != "token" || auth.Subscribe.Stage != "post_connect" ||
		!auth.Subscribe.Token.SecretConfigured {
		t.Errorf("media auth view = %+v", auth)
	}
	if !view.Effective.API.Auth.Enabled || !view.Effective.API.Auth.BearerTokenConfigured {
		t.Errorf("auth view = %+v", view.Effective.API.Auth)
	}
	if view.Effective.API.Console.Username != "admin" || !view.Effective.API.Console.PasswordConfigured || !view.Effective.API.Console.PasswordHashed {
		t.Errorf("console view = %+v", view.Effective.API.Console)
	}
	if view.Reload["server.name"] != config.ReloadHot || view.Reload["api.listen"] != config.ReloadRestart {
		t.Errorf("reload metadata = %+v", view.Reload)
	}
}

func TestGetConfigUsesOneCapturedSnapshotDuringApply(t *testing.T) {
	testServer := newConfigTestServer(t)
	before := testServer.manager.Current()
	applyStarted := make(chan struct{})
	releaseApply := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(releaseApply)
		}
	}()
	testServer.manager.SetApply(func(_ context.Context, _, next *config.Config, _ config.ChangeSet) error {
		if err := testServer.server.ApplyConfig(next); err != nil {
			return err
		}
		close(applyStarted)
		<-releaseApply
		return nil
	})

	body := bytes.NewBufferString(`{"current_password":"current-password","server":{"name":"applying"}}`)
	patchRequest, err := http.NewRequest(http.MethodPatch, testServer.addr+"/api/v1/config", body)
	if err != nil {
		t.Fatal(err)
	}
	patchRequest.Header.Set("Authorization", "Bearer "+testServer.bearerToken)
	patchRequest.Header.Set("Content-Type", "application/json")
	patchRequest.Header.Set("If-Match", strconv.Quote(before.Revision))
	type patchResult struct {
		response *http.Response
		err      error
	}
	patchDone := make(chan patchResult, 1)
	go func() {
		response, requestErr := http.DefaultClient.Do(patchRequest)
		patchDone <- patchResult{response: response, err: requestErr}
	}()

	select {
	case <-applyStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("config apply did not reach blocking point")
	}
	if got := testServer.server.Config().Server.Name; got != "applying" {
		t.Fatalf("server config = %q, want applying while manager is blocked", got)
	}

	getResponse := testServer.request(t, http.MethodGet, "/api/v1/config", nil)
	getBody := mustReadAll(t, getResponse.Body)
	getResponse.Body.Close()
	if got, want := getResponse.Header.Get("ETag"), strconv.Quote(before.Revision); got != want {
		t.Errorf("ETag = %q, want %q", got, want)
	}
	var view configView
	if err := json.Unmarshal(decodeAPIData(t, getBody), &view); err != nil {
		t.Fatal(err)
	}
	if view.Revision != before.Revision || view.Effective.Server.Name != "Config Test" {
		t.Errorf("captured response revision/name = %q/%q, want %q/Config Test", view.Revision, view.Effective.Server.Name, before.Revision)
	}

	close(releaseApply)
	released = true
	result := <-patchDone
	if result.err != nil {
		t.Fatal(result.err)
	}
	defer result.response.Body.Close()
	if result.response.StatusCode != http.StatusOK {
		responseBody, _ := io.ReadAll(result.response.Body)
		t.Fatalf("PATCH status = %d, want 200: %s", result.response.StatusCode, responseBody)
	}
}

func TestPatchMediaAuthOmittedSecretsArePreserved(t *testing.T) {
	testServer := newConfigTestServer(t)
	resp := testServer.bearerConfigPatch(t, `{
		"current_password":"current-password",
		"auth":{"publish":{"mode":"token","token":{"algorithm":"HS256"}}}
	}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}
	auth := testServer.server.Config().Auth
	if auth.Publish.Token.Secret != "publish-token-secret" || auth.Publish.Callback.URL != "https://auth.example/publish?key=publish-callback-secret" {
		t.Errorf("omitted secrets changed: token=%q callback=%q", auth.Publish.Token.Secret, auth.Publish.Callback.URL)
	}
}

func TestPatchMediaAuthNullSecretsArePreserved(t *testing.T) {
	testServer := newConfigTestServer(t)
	resp := testServer.bearerConfigPatch(t, `{
		"current_password":"current-password",
		"server":{"log_level":"debug"},
		"auth":{"publish":{"token":{"secret":null},"callback":{"url":null}}}
	}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}
	auth := testServer.server.Config().Auth.Publish
	if auth.Token.Secret != "publish-token-secret" || auth.Callback.URL != "https://auth.example/publish?key=publish-callback-secret" {
		t.Errorf("null secrets changed: token=%q callback=%q", auth.Token.Secret, auth.Callback.URL)
	}
}

func TestPatchMediaAuthNonEmptySecretsReplaceExistingValues(t *testing.T) {
	testServer := newConfigTestServer(t)
	resp := testServer.bearerConfigPatch(t, `{
		"current_password":"current-password",
		"auth":{"publish":{"token":{"secret":"replacement-token"},"callback":{"url":"https://new.example/authorize"}}}
	}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}
	auth := testServer.server.Config().Auth
	if auth.Publish.Token.Secret != "replacement-token" || auth.Publish.Callback.URL != "https://new.example/authorize" {
		t.Errorf("replaced secrets = token %q callback %q", auth.Publish.Token.Secret, auth.Publish.Callback.URL)
	}
}

func TestPatchMediaAuthEmptySecretsClearWhenRuleIsDisabled(t *testing.T) {
	testServer := newConfigTestServer(t)
	resp := testServer.bearerConfigPatch(t, `{
			"current_password":"current-password",
			"auth":{"publish":{"mode":"none","token":{"secret":""},"callback":{"url":""}}}
		}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}
	auth := testServer.server.Config().Auth
	if auth.Publish.Token.Secret != "" || auth.Publish.Callback.URL != "" {
		t.Errorf("cleared secrets = token %q callback %q", auth.Publish.Token.Secret, auth.Publish.Callback.URL)
	}
}

func TestPatchMediaAuthRejectsClearingCredentialUsedByActiveRule(t *testing.T) {
	testServer := newConfigTestServer(t)
	resp := testServer.bearerConfigPatch(t, `{
		"current_password":"current-password",
		"auth":{"publish":{"mode":"token","token":{"secret":""}}}
	}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 400: %s", resp.StatusCode, body)
	}
	if got := testServer.server.Config().Auth.Publish.Token.Secret; got != "publish-token-secret" {
		t.Errorf("active token secret changed to %q after rejected clear", got)
	}
}

func TestPatchConfigRejectsStaleETagWithoutUpdating(t *testing.T) {
	testServer := newConfigTestServer(t)
	originalRevision := testServer.manager.Current().Revision
	body := bytes.NewBufferString(`{"current_password":"current-password","server":{"name":"stale write"}}`)
	req, err := http.NewRequest(http.MethodPatch, testServer.addr+"/api/v1/config", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testServer.bearerToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("If-Match", `"sha256:stale"`)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPreconditionFailed {
		responseBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 412: %s", resp.StatusCode, responseBody)
	}
	if got := testServer.manager.Current().Revision; got != originalRevision {
		t.Errorf("revision changed from %q to %q", originalRevision, got)
	}
	if got := testServer.server.Config().Server.Name; got != "Config Test" {
		t.Errorf("server name changed to %q", got)
	}
}

func TestCredentialPatchRequiresCurrentPassword(t *testing.T) {
	testServer := newConfigTestServer(t)
	snapshot := testServer.manager.Current()
	body := bytes.NewBufferString(`{"current_password":"wrong-password","api":{"auth":{"bearer_token":"unauthorized-token"}}}`)
	req, err := http.NewRequest(http.MethodPatch, testServer.addr+"/api/v1/config", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testServer.bearerToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("If-Match", `"`+snapshot.Revision+`"`)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		responseBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 403: %s", resp.StatusCode, responseBody)
	}
	if got := testServer.manager.Current().Revision; got != snapshot.Revision {
		t.Errorf("revision changed from %q to %q", snapshot.Revision, got)
	}
	if got := testServer.server.Config().API.Auth.BearerToken; got != testServer.bearerToken {
		t.Errorf("bearer token changed after rejected credential patch")
	}
}

func TestBearerNonCredentialPatchDoesNotRequireCurrentPassword(t *testing.T) {
	testServer := newConfigTestServer(t)
	snapshot := testServer.manager.Current()
	body := bytes.NewBufferString(`{"server":{"name":"bearer-only update"}}`)
	req, err := http.NewRequest(http.MethodPatch, testServer.addr+"/api/v1/config", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testServer.bearerToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("If-Match", strconv.Quote(snapshot.Revision))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		responseBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, responseBody)
	}
	assertPrivateConfigResponse(t, resp)
	if got := testServer.server.Config().Server.Name; got != "bearer-only update" {
		t.Errorf("server name = %q, want bearer-only update", got)
	}
}

func TestPatchConfigUpdatesThroughManagerAndReturnsNewETag(t *testing.T) {
	testServer := newConfigTestServer(t)
	originalRevision := testServer.manager.Current().Revision
	body := bytes.NewBufferString(`{
		"current_password":"current-password",
		"server":{"name":"Updated Server","log_level":"debug"},
		"api":{
			"pprof_enabled":true,
			"auth":{"enabled":true,"bearer_token":"rotated-token"},
			"console":{"username":"operator"}
		}
	}`)
	req, err := http.NewRequest(http.MethodPatch, testServer.addr+"/api/v1/config", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testServer.bearerToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("If-Match", `"`+originalRevision+`"`)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		responseBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, responseBody)
	}

	updated := testServer.manager.Current()
	if updated.Revision == originalRevision {
		t.Fatal("manager revision did not change")
	}
	if got, want := resp.Header.Get("ETag"), `"`+updated.Revision+`"`; got != want {
		t.Errorf("ETag = %q, want %q", got, want)
	}
	cfg := testServer.server.Config()
	if cfg.Server.Name != "Updated Server" || cfg.Server.LogLevel != "debug" {
		t.Errorf("server config = %+v", cfg.Server)
	}
	if !cfg.API.PprofEnabled || cfg.API.Auth.BearerToken != "rotated-token" || cfg.API.Console.Username != "operator" {
		t.Errorf("API config = %+v", cfg.API)
	}

	for token, want := range map[string]int{
		testServer.bearerToken: http.StatusUnauthorized,
		"rotated-token":        http.StatusOK,
	} {
		check, err := http.NewRequest(http.MethodGet, testServer.addr+"/api/v1/config", nil)
		if err != nil {
			t.Fatal(err)
		}
		check.Header.Set("Authorization", "Bearer "+token)
		checkResp, err := http.DefaultClient.Do(check)
		if err != nil {
			t.Fatal(err)
		}
		checkResp.Body.Close()
		if checkResp.StatusCode != want {
			t.Errorf("token %q status = %d, want %d", token, checkResp.StatusCode, want)
		}
	}
}

func TestGetConfigIssuesCSRFTokenForSession(t *testing.T) {
	testServer := newConfigTestServer(t)
	sessionCookie := testServer.login(t, testServer.password)
	_, csrfToken := testServer.sessionConfig(t, sessionCookie)
	if csrfToken == "" {
		t.Fatal("session config response did not include a CSRF token")
	}
}

func TestSessionConfigPatchRequiresSameOriginAndCSRF(t *testing.T) {
	tests := []struct {
		name       string
		origin     func(configTestServer) string
		csrfHeader func(string) string
	}{
		{name: "missing origin", csrfHeader: func(token string) string { return token }},
		{name: "cross-site origin", origin: func(configTestServer) string { return "https://attacker.example" }, csrfHeader: func(token string) string { return token }},
		{name: "missing CSRF", origin: func(server configTestServer) string { return server.addr }},
		{name: "invalid CSRF", origin: func(server configTestServer) string { return server.addr }, csrfHeader: func(string) string { return "invalid" }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			testServer := newConfigTestServer(t)
			sessionCookie := testServer.login(t, testServer.password)
			etag, csrfToken := testServer.sessionConfig(t, sessionCookie)
			origin := ""
			if test.origin != nil {
				origin = test.origin(testServer)
			}
			csrfHeader := ""
			if test.csrfHeader != nil {
				csrfHeader = test.csrfHeader(csrfToken)
			}

			resp := testServer.sessionPatch(t, sessionCookie, etag, origin, csrfHeader, `{
				"current_password":"current-password",
				"server":{"name":"cross-site write"}
			}`)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want 403: %s", resp.StatusCode, body)
			}
			if got := testServer.server.Config().Server.Name; got != "Config Test" {
				t.Errorf("server name changed to %q", got)
			}
		})
	}
}

func TestSessionConfigPatchAcceptsSameOriginAndCSRF(t *testing.T) {
	testServer := newConfigTestServer(t)
	sessionCookie := testServer.login(t, testServer.password)
	etag, csrfToken := testServer.sessionConfig(t, sessionCookie)
	resp := testServer.sessionPatch(t, sessionCookie, etag, testServer.addr, csrfToken, `{
		"current_password":"current-password",
		"server":{"name":"session update"}
	}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}
	if got := testServer.server.Config().Server.Name; got != "session update" {
		t.Errorf("server name = %q, want session update", got)
	}
}

func TestPasswordChangeRejectsStaleETag(t *testing.T) {
	testServer := newConfigTestServer(t)
	sessionCookie := testServer.login(t, testServer.password)
	_, csrfToken := testServer.sessionConfig(t, sessionCookie)
	originalHash := testServer.server.Config().API.Console.PasswordHash
	resp := testServer.passwordChange(t, sessionCookie, `"sha256:stale"`, testServer.addr, csrfToken, testServer.password, "replacement-password")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPreconditionFailed {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 412: %s", resp.StatusCode, body)
	}
	if got := testServer.server.Config().API.Console.PasswordHash; got != originalHash {
		t.Error("password hash changed after stale write")
	}
}

func TestPasswordChangeHashesPasswordAndRevokesSessions(t *testing.T) {
	testServer := newConfigTestServer(t)
	sessionCookie := testServer.login(t, testServer.password)
	etag, csrfToken := testServer.sessionConfig(t, sessionCookie)
	const newPassword = "replacement-password"
	resp := testServer.passwordChange(t, sessionCookie, etag, testServer.addr, csrfToken, testServer.password, newPassword)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}
	assertPrivateConfigResponse(t, resp)
	body := mustReadAll(t, resp.Body)
	if strings.Contains(string(body), newPassword) || strings.Contains(string(body), "$2") {
		t.Fatalf("password response leaked credential material: %s", body)
	}

	console := testServer.server.Config().API.Console
	if console.Password != "" {
		t.Errorf("plaintext password = %q, want empty", console.Password)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(console.PasswordHash), []byte(newPassword)); err != nil {
		t.Errorf("new password does not match stored hash: %v", err)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(console.PasswordHash), []byte(testServer.password)); err == nil {
		t.Error("old password still matches stored hash")
	}
	if cost, err := bcrypt.Cost([]byte(console.PasswordHash)); err != nil || cost != bcrypt.DefaultCost {
		t.Errorf("bcrypt cost = %d, %v; want %d", cost, err, bcrypt.DefaultCost)
	}

	oldSessionReq, err := http.NewRequest(http.MethodGet, testServer.addr+"/api/v1/config", nil)
	if err != nil {
		t.Fatal(err)
	}
	oldSessionReq.AddCookie(sessionCookie)
	oldSessionResp, err := http.DefaultClient.Do(oldSessionReq)
	if err != nil {
		t.Fatal(err)
	}
	oldSessionResp.Body.Close()
	if oldSessionResp.StatusCode != http.StatusUnauthorized {
		t.Errorf("old session status = %d, want 401", oldSessionResp.StatusCode)
	}

	if got := testServer.loginStatus(t, testServer.password); got != http.StatusUnauthorized {
		t.Errorf("old password login status = %d, want 401", got)
	}
	if got := testServer.loginStatus(t, newPassword); got != http.StatusSeeOther {
		t.Errorf("new password login status = %d, want 303", got)
	}
}

func TestPasswordChangeKeepsSessionIssuedAfterCredentialPublication(t *testing.T) {
	testServer := newConfigTestServer(t)
	oldSession := testServer.login(t, testServer.password)
	etag, csrfToken := testServer.sessionConfig(t, oldSession)
	applyPublished := make(chan struct{})
	releaseApply := make(chan struct{})
	released := false
	t.Cleanup(func() {
		if !released {
			close(releaseApply)
		}
	})
	testServer.manager.SetApply(func(_ context.Context, _, next *config.Config, _ config.ChangeSet) error {
		if err := testServer.server.ApplyConfig(next); err != nil {
			return err
		}
		close(applyPublished)
		<-releaseApply
		return nil
	})

	const newPassword = "concurrent-new-password"
	payload, err := json.Marshal(map[string]string{
		"current_password": testServer.password,
		"new_password":     newPassword,
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, testServer.addr+"/api/v1/config/password", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(oldSession)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("If-Match", etag)
	req.Header.Set("Origin", testServer.addr)
	req.Header.Set("X-CSRF-Token", csrfToken)
	type passwordChangeResult struct {
		response *http.Response
		err      error
	}
	changeDone := make(chan passwordChangeResult, 1)
	go func() {
		response, requestErr := http.DefaultClient.Do(req)
		changeDone <- passwordChangeResult{response: response, err: requestErr}
	}()

	select {
	case <-applyPublished:
	case <-time.After(3 * time.Second):
		t.Fatal("password change did not publish credentials")
	}
	newSession := testServer.login(t, newPassword)
	close(releaseApply)
	released = true
	result := <-changeDone
	if result.err != nil {
		t.Fatal(result.err)
	}
	defer result.response.Body.Close()
	if result.response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(result.response.Body)
		t.Fatalf("password change status = %d, want 200: %s", result.response.StatusCode, body)
	}

	testServer.sessionConfig(t, newSession)
}

func assertPrivateConfigResponse(t *testing.T, resp *http.Response) {
	t.Helper()
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	for _, value := range []string{"Cookie", "Authorization"} {
		if !strings.Contains(resp.Header.Get("Vary"), value) {
			t.Errorf("Vary = %q, want %s", resp.Header.Get("Vary"), value)
		}
	}
}

func (s configTestServer) sessionConfig(t *testing.T, cookie *http.Cookie) (etag, csrfToken string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, s.addr+"/api/v1/config", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}
	data := decodeAPIData(t, mustReadAll(t, resp.Body))
	var view struct {
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.Unmarshal(data, &view); err != nil {
		t.Fatal(err)
	}
	return resp.Header.Get("ETag"), view.CSRFToken
}

func (s configTestServer) bearerConfigPatch(t *testing.T, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPatch, s.addr+"/api/v1/config", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+s.bearerToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("If-Match", `"`+s.manager.Current().Revision+`"`)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func (s configTestServer) sessionPatch(t *testing.T, cookie *http.Cookie, etag, origin, csrfToken, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPatch, s.addr+"/api/v1/config", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(cookie)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("If-Match", etag)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if csrfToken != "" {
		req.Header.Set("X-CSRF-Token", csrfToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func (s configTestServer) passwordChange(t *testing.T, cookie *http.Cookie, etag, origin, csrfToken, currentPassword, newPassword string) *http.Response {
	t.Helper()
	payload, err := json.Marshal(map[string]string{
		"current_password": currentPassword,
		"new_password":     newPassword,
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, s.addr+"/api/v1/config/password", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(cookie)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("If-Match", etag)
	req.Header.Set("Origin", origin)
	req.Header.Set("X-CSRF-Token", csrfToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func (s configTestServer) loginStatus(t *testing.T, password string) int {
	t.Helper()
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.PostForm(s.addr+"/console/login", url.Values{
		"username": {"admin"},
		"password": {password},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func mustReadAll(t *testing.T, reader io.Reader) []byte {
	t.Helper()
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
