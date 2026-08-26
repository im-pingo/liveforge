package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	configruntime "github.com/im-pingo/liveforge/config/runtime"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/protocoltest"
	"gopkg.in/yaml.v3"
)

func TestConfigAndProtocolReadAccessUsesViewerRBAC(t *testing.T) {
	cfg := newTestConfig()
	cfg.API.Auth.Tokens = []config.APIAuthToken{
		{Name: "viewer", Token: "viewer-token", Role: "viewer"},
		{Name: "operator", Token: "operator-token", Role: "operator"},
	}
	server := core.NewServer(cfg)
	server.RegisterModule(&protocolRunnerModule{name: "sipgateway"})
	server.RegisterAPIHandler("GET /api/v1/gb28181/test", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, protocoltest.New("gb28181", []protocoltest.Check{{Name: "route", Passed: true}}))
	}))
	audit := NewAuditStore(16)
	mux := http.NewServeMux()
	registerRoutes(mux, server, audit)
	handler := buildSecurityHandler(mux, server, audit)

	tests := []struct {
		name   string
		method string
		path   string
		body   string
		status int
	}{
		{name: "config document", method: http.MethodGet, path: "/api/v1/server/config/document", status: http.StatusOK},
		{name: "config schema", method: http.MethodGet, path: "/api/v1/server/config/schema", status: http.StatusOK},
		{name: "config validate", method: http.MethodPost, path: "/api/v1/server/config/validate", body: "server:\n  name: test\n", status: http.StatusOK},
		{name: "sip self-test", method: http.MethodGet, path: "/api/v1/sipgateway/test", status: http.StatusOK},
		{name: "gb28181 self-test", method: http.MethodGet, path: "/api/v1/gb28181/test", status: http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
			req.Header.Set("Authorization", "Bearer viewer-token")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			if w.Code != tt.status {
				t.Fatalf("viewer %s status=%d want=%d body=%s", tt.path, w.Code, tt.status, w.Body.String())
			}
		})
	}

	for _, path := range []string{"/api/v1/server/config/refresh", "/api/v1/server/config/apply"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("server:\n  name: test\n"))
		req.Header.Set("Authorization", "Bearer viewer-token")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("viewer %s status=%d want=403 body=%s", path, w.Code, w.Body.String())
		}
	}

	for _, path := range []string{"/api/v1/server/config/refresh", "/api/v1/server/config/apply"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("server:\n  name: test\n"))
		req.Header.Set("Authorization", "Bearer operator-token")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code == http.StatusForbidden || w.Code == http.StatusUnauthorized {
			t.Fatalf("operator %s status=%d unexpectedly denied body=%s", path, w.Code, w.Body.String())
		}
	}
}

func TestHandleConfigApplyWritesFileAndPreservesRedactedSecrets(t *testing.T) {
	cfg := config.Defaults()
	cfg.API.Auth.BearerToken = "api-secret"
	cfg.API.Console.Username = "admin"
	cfg.API.Console.Role = "admin"
	cfg.API.Console.Password = "console-secret"
	document, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "liveforge.yaml")
	if err := os.WriteFile(path, document, 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := configruntime.NewFileSource(path)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := configruntime.NewManager(configruntime.Options{
		Source:       source,
		Initial:      cfg,
		PollInterval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	h, server := newTestHandlers(t)
	server.UpdateConfig(cfg)
	server.SetConfigManager(manager)
	// The editor sends a complete document with sensitive values redacted.
	var redactedMap map[string]any
	if err := yaml.Unmarshal(document, &redactedMap); err != nil {
		t.Fatal(err)
	}
	redactedMap["server"].(map[string]any)["name"] = "edited"
	redactedMap["api"].(map[string]any)["auth"].(map[string]any)["bearer_token"] = "[REDACTED]"
	redactedMap["api"].(map[string]any)["console"].(map[string]any)["password"] = "[REDACTED]"
	redacted, err := yaml.Marshal(redactedMap)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/server/config/apply", strings.NewReader(string(redacted)))
	request.Header.Set("Content-Type", "application/yaml")
	w := httptest.NewRecorder()
	h.handleConfigApply(w, request)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}

	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := configruntime.ValidateDocument(written)
	if err != nil {
		t.Fatalf("written document is invalid: %v\n%s", err, written)
	}
	if parsed.Server.Name != "edited" || parsed.API.Auth.BearerToken != "api-secret" || parsed.API.Console.Password != "console-secret" {
		t.Fatalf("written config lost edits or secrets: server=%q bearer=%q password=%q", parsed.Server.Name, parsed.API.Auth.BearerToken, parsed.API.Console.Password)
	}
}

func TestPreserveRedactedSecretsRestoresArrayValues(t *testing.T) {
	current := config.Defaults()
	current.API.Auth.Tokens = []config.APIAuthToken{{Name: "admin", Token: "admin-secret", Role: "admin"}}
	current.WebRTC.ICEServers = []config.ICEServer{{URLs: []string{"turn:turn.example.test"}, Username: "turn-user", Credential: "turn-secret"}}
	document := []byte(`api:
  auth:
    tokens:
      - name: admin
        token: "[REDACTED]"
        role: admin
webrtc:
  ice_servers:
    - urls:
        - turn:turn.example.test
      username: turn-user
      credential: "[REDACTED]"
`)
	restored, err := preserveRedactedSecrets(document, current)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := configruntime.ValidateDocument(restored)
	if err != nil {
		t.Fatalf("restored document is invalid: %v\n%s", err, restored)
	}
	if got := parsed.API.Auth.Tokens[0].Token; got != "admin-secret" {
		t.Fatalf("token = %q, want restored secret", got)
	}
	if got := parsed.WebRTC.ICEServers[0].Credential; got != "turn-secret" {
		t.Fatalf("ICE credential = %q, want restored secret", got)
	}
}

func TestPreserveRedactedSecretsKeepsSourceCommentsAndUnknownFields(t *testing.T) {
	current := config.Defaults()
	current.API.Auth.BearerToken = "api-secret"
	document := []byte("# keep source comment\napi:\n  auth:\n    bearer_token: [REDACTED]\ncustom_runtime_field: retained\n")
	restored, err := preserveRedactedSecrets(document, current)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(restored), "# keep source comment") {
		t.Fatalf("source comment was dropped: %q", restored)
	}
	if !strings.Contains(string(restored), "custom_runtime_field: retained") {
		t.Fatalf("unknown source field was dropped: %q", restored)
	}
	parsed, err := configruntime.ValidateDocument(restored)
	if err != nil {
		t.Fatalf("restored document is invalid: %v", err)
	}
	if parsed.API.Auth.BearerToken != "api-secret" {
		t.Fatalf("bearer token = %q, want restored secret", parsed.API.Auth.BearerToken)
	}
}

func TestHandleConfigApplyRejectsInvalidDocument(t *testing.T) {
	h, server := newTestHandlers(t)
	manager, err := configruntime.NewManager(configruntime.Options{Source: testConfigSource{}, Initial: server.Config()})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	server.SetConfigManager(manager)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/server/config/apply", strings.NewReader("server:\n  name: ["))
	request.Header.Set("Content-Type", "application/yaml")
	w := httptest.NewRecorder()
	h.handleConfigApply(w, request)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestHandleConfigApplyRejectsReadOnlySource(t *testing.T) {
	h, server := newTestHandlers(t)
	manager, err := configruntime.NewManager(configruntime.Options{Source: testConfigSource{}, Initial: server.Config()})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	server.SetConfigManager(manager)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/server/config/apply", strings.NewReader("server:\n  name: edited\n"))
	request.Header.Set("Content-Type", "application/yaml")
	w := httptest.NewRecorder()
	h.handleConfigApply(w, request)
	if w.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var response apiResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Message == "" {
		t.Fatal("read-only error did not include a message")
	}
}

func TestConfigDocumentPreservesRawSourceFieldsAndComments(t *testing.T) {
	const sourceDocument = "# keep this source comment\nserver:\n  name: liveforge\napi:\n  auth:\n    bearer_token: source-secret\ncustom_runtime_field: retained\n"
	h, server := newTestHandlers(t)
	manager, err := configruntime.NewManager(configruntime.Options{
		Source:  &rawDocumentSource{document: []byte(sourceDocument)},
		Initial: nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if snapshot := manager.Snapshot(); snapshot == nil || string(snapshot.DesiredDocument) != sourceDocument {
		t.Fatalf("manager did not retain source document: %+v", snapshot)
	}
	server.SetConfigManager(manager)

	w := httptest.NewRecorder()
	h.handleConfigDocument(w, httptest.NewRequest(http.MethodGet, "/api/v1/server/config/document", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	data := decodeAPIData(t, w.Body.Bytes())
	var response struct {
		Desired     map[string]any `json:"desired"`
		DesiredText string         `json:"desired_document"`
		Schema      map[string]any `json:"schema"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		t.Fatal(err)
	}
	if response.Desired["custom_runtime_field"] != "retained" {
		t.Fatalf("raw source field was dropped: %+v", response.Desired)
	}
	if !strings.Contains(response.DesiredText, "# keep this source comment") {
		t.Fatalf("source comment was dropped: %q", response.DesiredText)
	}
	if strings.Contains(response.DesiredText, "source-secret") || !strings.Contains(response.DesiredText, "[REDACTED]") {
		t.Fatalf("source secret was not redacted: %q", response.DesiredText)
	}
	if response.Schema["$defs"] == nil {
		t.Fatal("document response did not include the full JSON Schema")
	}
}

func TestHandleConfigValidateAcceptsDocumentContentTypes(t *testing.T) {
	h, _ := newTestHandlers(t)
	for name, contentType := range map[string]string{
		"yaml":          "application/yaml",
		"json envelope": "application/json",
	} {
		t.Run(name, func(t *testing.T) {
			body := "server:\n  name: liveforge\n"
			if contentType == "application/json" {
				body = `{"document":"server:\n  name: liveforge\n"}`
			}
			req := httptest.NewRequest(http.MethodPost, "/api/v1/server/config/validate", strings.NewReader(body))
			req.Header.Set("Content-Type", contentType)
			w := httptest.NewRecorder()
			h.handleConfigValidate(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestEmbeddedConfigSchemaMatchesRepository(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	wantBytes, err := os.ReadFile(filepath.Join(filepath.Dir(filename), "..", "..", "docs", "config", "config.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var want, got any
	if err := json.Unmarshal(wantBytes, &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(embeddedConfigSchema, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatal("embedded schema differs from docs/config/config.schema.json")
	}
	if !bytes.Equal(bytes.TrimSpace(wantBytes), bytes.TrimSpace(embeddedConfigSchema)) {
		t.Fatal("embedded schema formatting differs; update the embedded asset from the canonical schema")
	}
}

func TestRedactedSourceDetailsRemoveURLCredentials(t *testing.T) {
	details := redactedSourceDetails(config.RuntimeConfig{
		HTTP:   config.RuntimeHTTPSourceConfig{URL: "https://user:password@config.example.test/live.yaml?token=secret"},
		Consul: config.RuntimeConsulSourceConfig{Address: "http://token:secret@consul.example.test:8500?auth=secret"},
	})
	httpDetails := details["http"].(map[string]any)
	consulDetails := details["consul"].(map[string]any)
	if got := httpDetails["url"]; got != "https://config.example.test/live.yaml" {
		t.Fatalf("redacted HTTP URL = %q", got)
	}
	if got := consulDetails["address"]; got != "http://consul.example.test:8500" {
		t.Fatalf("redacted Consul address = %q", got)
	}
}

type rawDocumentSource struct {
	document []byte
}

func (s *rawDocumentSource) Load(context.Context, configruntime.Version) (configruntime.Snapshot, error) {
	return configruntime.Snapshot{Data: append([]byte(nil), s.document...), Version: "raw-1"}, nil
}

func (s *rawDocumentSource) Close() error { return nil }
