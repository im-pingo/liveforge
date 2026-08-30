package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

func TestHandleConfigApplyWritesFileAndPreservesRedactedSecretsAndUnmappedFields(t *testing.T) {
	cfg := config.Defaults()
	cfg.API.Auth.BearerToken = "api-secret"
	cfg.API.Console.Username = "admin"
	cfg.API.Console.Role = "admin"
	cfg.API.Console.Password = "console-secret"
	document, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	document = append(document, []byte("custom_runtime_field: retained\n")...)
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
	if startErr := manager.Start(context.Background()); startErr != nil {
		t.Fatal(startErr)
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
	if !strings.Contains(string(written), "custom_runtime_field: retained") {
		t.Fatalf("written config dropped unmapped desired-source field:\n%s", written)
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

func TestPreserveRedactedSecretsRestoresUnknownOneElementSensitiveSequence(t *testing.T) {
	const sourceDocument = "custom_private_keys: [secret-value]\n"
	redacted, err := redactedConfigDocument([]byte(sourceDocument))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(redacted), "secret-value") {
		t.Fatalf("redacted document leaked the source secret: %s", redacted)
	}

	restored, err := preserveRedactedSecretsWithDocument(redacted, config.Defaults(), []byte(sourceDocument))
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := yaml.Unmarshal(restored, &document); err != nil {
		t.Fatal(err)
	}
	values, ok := document["custom_private_keys"].([]any)
	if !ok || len(values) != 1 || values[0] != "secret-value" {
		t.Fatalf("restored custom_private_keys = %#v, want original one-element sequence", document["custom_private_keys"])
	}
}

func TestPreserveRedactedSecretsRejectsUnknownSensitiveSequenceWithoutUniqueOriginal(t *testing.T) {
	tests := []struct {
		name            string
		currentDocument string
	}{
		{name: "missing original"},
		{name: "ambiguous original", currentDocument: "custom_private_keys: [first-secret, second-secret]\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := []byte("custom_private_keys: [\"[REDACTED]\"]\n")
			if _, err := preserveRedactedSecretsWithDocument(candidate, config.Defaults(), []byte(test.currentDocument)); err == nil {
				t.Fatal("redacted sensitive sequence was accepted without a unique original")
			}
		})
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

func TestHandleConfigApplyRedactsSourceURLFromWriteError(t *testing.T) {
	h, server := newTestHandlers(t)
	const sourceURL = "https://config-user:config-password@config.example.test/live.yaml?token=query-secret" //nolint:gosec // Synthetic value verifies redaction.
	manager, err := configruntime.NewManager(configruntime.Options{
		Source:  errorConfigWriterSource{err: errors.New("write " + sourceURL + ": connection refused")},
		Initial: server.Config(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	server.SetConfigManager(manager)

	request := httptest.NewRequest(http.MethodPost, "/api/v1/server/config/apply", strings.NewReader("server:\n  name: edited\n"))
	request.Header.Set("Content-Type", "application/yaml")
	w := httptest.NewRecorder()
	h.handleConfigApply(w, request)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	for _, secret := range []string{"config-user", "config-password", "query-secret", "token="} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatalf("config apply error leaked %q: %s", secret, w.Body.String())
		}
	}
	if !strings.Contains(w.Body.String(), "config.example.test") {
		t.Fatalf("redacted error lost useful endpoint identity: %s", w.Body.String())
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
	if startErr := manager.Start(context.Background()); startErr != nil {
		t.Fatal(startErr)
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
	if unmarshalErr := json.Unmarshal(data, &response); unmarshalErr != nil {
		t.Fatal(unmarshalErr)
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

func TestHandleConfigDocumentRedactsUnmappedHierarchicalURLValues(t *testing.T) {
	const sourceDocument = `server:
  name: liveforge
primary: https://hooks.slack.com/services/T111/B111/scalar-path-token
mirrors:
  - https://hooks.slack.com/services/T222/B222/sequence-path-token
ordinary:
  - 30s
  - camera-001
  - relay.example.test:443
  - 239.0.0.1
  - turn:relay.example.test:3478?transport=udp
`
	h, server := newTestHandlers(t)
	manager, err := configruntime.NewManager(configruntime.Options{
		Source:  &rawDocumentSource{document: []byte(sourceDocument)},
		Initial: nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if startErr := manager.Start(context.Background()); startErr != nil {
		t.Fatal(startErr)
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
	}
	if unmarshalErr := json.Unmarshal(data, &response); unmarshalErr != nil {
		t.Fatal(unmarshalErr)
	}
	desiredJSON, err := json.Marshal(response.Desired)
	if err != nil {
		t.Fatal(err)
	}
	for _, surface := range []struct {
		name string
		text string
	}{
		{name: "decoded desired", text: string(desiredJSON)},
		{name: "desired_document", text: response.DesiredText},
	} {
		t.Run(surface.name, func(t *testing.T) {
			for _, secret := range []string{"T111", "B111", "scalar-path-token", "T222", "B222", "sequence-path-token"} {
				if strings.Contains(surface.text, secret) {
					t.Errorf("management response leaked unmapped URL path credential %q: %s", secret, surface.text)
				}
			}
			if !strings.Contains(surface.text, "hooks.slack.com") || !strings.Contains(surface.text, redactedURLPathPrefix) {
				t.Errorf("management response lost safe URL host or digest marker: %s", surface.text)
			}
			for _, ordinary := range []string{"30s", "camera-001", "relay.example.test:443", "239.0.0.1", "turn:relay.example.test:3478?transport=udp"} {
				if !strings.Contains(surface.text, ordinary) {
					t.Errorf("management response changed ordinary value %q: %s", ordinary, surface.text)
				}
			}
		})
	}
}

func TestHandleConfigDocumentRehashesLiteralRedactedPathMarker(t *testing.T) {
	//nolint:gosec // Intentional fake URL credentials verify management-response redaction.
	const sourceURL = "https://literal-user:literal-password@hooks.slack.com:8443/__liveforge_redacted_path__/0123456789abcdef0123456789abcdef?token=literal-query#literal-fragment"
	const redactedURL = "https://REDACTED@hooks.slack.com:8443/__liveforge_redacted_path__/3b08eb10aa25a39ac0cf6bf776391a6b?__liveforge_redacted__=1"
	const sourceDocument = "server:\n  name: liveforge\nprimary: " + sourceURL + "\n"
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
	}
	if err := json.Unmarshal(data, &response); err != nil {
		t.Fatal(err)
	}
	var desiredDocument map[string]any
	if err := yaml.Unmarshal([]byte(response.DesiredText), &desiredDocument); err != nil {
		t.Fatal(err)
	}
	for _, surface := range []struct {
		name  string
		value any
	}{
		{name: "decoded desired", value: response.Desired["primary"]},
		{name: "desired_document", value: desiredDocument["primary"]},
	} {
		t.Run(surface.name, func(t *testing.T) {
			if surface.value != redactedURL {
				t.Fatalf("management response URL = %q, want source-path digest %q", surface.value, redactedURL)
			}
			if strings.Contains(surface.value.(string), "/__liveforge_redacted_path__/0123456789abcdef0123456789abcdef") {
				t.Fatalf("management response leaked literal marker-shaped source path: %q", surface.value)
			}
		})
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

func TestHandleConfigValidateDoesNotExpandProcessEnvironment(t *testing.T) {
	const processSecret = "viewer-must-not-read-this-process-secret"
	t.Setenv("LIVEFORGE_VALIDATE_PROCESS_SECRET", processSecret)
	h, _ := newTestHandlers(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/server/config/validate", strings.NewReader("server:\n  name: \"${LIVEFORGE_VALIDATE_PROCESS_SECRET}\"\n"))
	req.Header.Set("Content-Type", "application/yaml")
	w := httptest.NewRecorder()

	h.handleConfigValidate(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), processSecret) {
		t.Fatalf("validate response disclosed process environment value: %s", w.Body.String())
	}
	data := decodeAPIData(t, w.Body.Bytes())
	var response struct {
		Config map[string]any `json:"config"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		t.Fatal(err)
	}
	if got := response.Config["server"].(map[string]any)["name"]; got != "${LIVEFORGE_VALIDATE_PROCESS_SECRET}" {
		t.Fatalf("validated server.name=%q, want literal environment reference", got)
	}
}

func TestHandleConfigValidateRejectsUnknownKeys(t *testing.T) {
	h, _ := newTestHandlers(t)
	tests := []struct {
		name     string
		document string
		field    string
	}{
		{name: "top level", document: "servre:\n  name: typo\n", field: "servre"},
		{name: "nested", document: "server:\n  naem: typo\n", field: "naem"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/server/config/validate", strings.NewReader(test.document))
			req.Header.Set("Content-Type", "application/yaml")
			w := httptest.NewRecorder()

			h.handleConfigValidate(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), test.field) {
				t.Fatalf("unknown-field error did not identify %q: %s", test.field, w.Body.String())
			}
		})
	}
}

func TestHandleConfigValidateRejectsSecondYAMLDocument(t *testing.T) {
	h, _ := newTestHandlers(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/server/config/validate", strings.NewReader("server:\n  name: liveforge\n---\nmalicious_or_unknown:\n  value: ignored\n"))
	req.Header.Set("Content-Type", "application/yaml")
	w := httptest.NewRecorder()

	h.handleConfigValidate(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
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
		File:   config.RuntimeFileSourceConfig{MaxBytes: 11},
		HTTP:   config.RuntimeHTTPSourceConfig{URL: "https://user:password@config.example.test/live.yaml?token=secret"},
		Consul: config.RuntimeConsulSourceConfig{Address: "http://token:secret@consul.example.test:8500?auth=secret"},
		Redis:  config.RuntimeRedisSourceConfig{MaxBytes: 13},
	})
	httpDetails := details["http"].(map[string]any)
	consulDetails := details["consul"].(map[string]any)
	if got := details["file"].(map[string]any)["max_bytes"]; got != int64(11) {
		t.Fatalf("redacted file max_bytes = %v, want 11", got)
	}
	if got := details["redis"].(map[string]any)["max_bytes"]; got != int64(13) {
		t.Fatalf("redacted Redis max_bytes = %v, want 13", got)
	}
	if got := httpDetails["url"].(string); !strings.Contains(got, "config.example.test") ||
		!strings.Contains(got, redactedURLPathPrefix) || strings.Contains(got, "live.yaml") {
		t.Fatalf("redacted HTTP URL = %q, want visible host and opaque path", got)
	}
	if got := consulDetails["address"]; got != "http://consul.example.test:8500" {
		t.Fatalf("redacted Consul address = %q", got)
	}
}

func TestConfigSecretRedactionCoversAPIKeyAndSchemaSecretFields(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal(embeddedConfigSchema, &schema); err != nil {
		t.Fatal(err)
	}
	secretProperties := make(map[string]struct{})
	var collect func(map[string]any)
	collect = func(node map[string]any) {
		if properties, ok := node["properties"].(map[string]any); ok {
			for name, raw := range properties {
				child, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				if secret, _ := child["x-liveforge-secret"].(bool); secret {
					secretProperties[name] = struct{}{}
				}
				collect(child)
			}
		}
		if definitions, ok := node["$defs"].(map[string]any); ok {
			for _, raw := range definitions {
				if child, ok := raw.(map[string]any); ok {
					collect(child)
				}
			}
		}
		if items, ok := node["items"].(map[string]any); ok {
			collect(items)
		}
	}
	collect(schema)
	for key := range secretProperties {
		if !isSensitiveConfigKey(key) {
			t.Errorf("schema x-liveforge-secret property %q is not classified as sensitive", key)
		}
	}
	if !isSensitiveConfigKey("api_key") {
		t.Error("api_key is not classified as sensitive")
	}

	const sourceDocument = `tls:
  cert_file: /etc/liveforge/public-cert.pem
  key_file: /etc/liveforge/private-key-material.pem
custom_service:
  api_key: custom-api-key-material
`
	redacted, err := redactedConfigDocument([]byte(sourceDocument))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private-key-material", "custom-api-key-material"} {
		if strings.Contains(string(redacted), secret) {
			t.Fatalf("redacted document leaked %q: %s", secret, redacted)
		}
	}
	restored, err := preserveRedactedSecretsWithDocument(redacted, config.Defaults(), []byte(sourceDocument))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private-key-material", "custom-api-key-material"} {
		if !strings.Contains(string(restored), secret) {
			t.Fatalf("restored document lost %q: %s", secret, restored)
		}
	}
}

func TestConfigURLPathCredentialsAreOpaqueAndRestoreByStableIdentity(t *testing.T) {
	const sourceDocument = `custom_callback_urls:
  - https://hooks.slack.com/services/T111/B111/first-path-token
  - https://hooks.slack.com/services/T222/B222/second-path-token
`
	redacted, err := redactedConfigDocument([]byte(sourceDocument))
	if err != nil {
		t.Fatal(err)
	}
	redactedText := string(redacted)
	for _, secret := range []string{"T111", "B111", "first-path-token", "T222", "B222", "second-path-token"} {
		if strings.Contains(redactedText, secret) {
			t.Fatalf("redacted document leaked URL path credential %q: %s", secret, redacted)
		}
	}
	if !strings.Contains(redactedText, "hooks.slack.com") {
		t.Fatalf("redacted document lost safe URL host: %s", redacted)
	}

	var candidate map[string]any
	if unmarshalErr := yaml.Unmarshal(redacted, &candidate); unmarshalErr != nil {
		t.Fatal(unmarshalErr)
	}
	reverseConfigSequence(t, candidate, "custom_callback_urls")
	candidateDocument, err := yaml.Marshal(candidate)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := preserveRedactedSecretsWithDocument(candidateDocument, config.Defaults(), []byte(sourceDocument))
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := yaml.Unmarshal(restored, &document); err != nil {
		t.Fatal(err)
	}
	urls := document["custom_callback_urls"].([]any)
	if len(urls) != 2 || urls[0] != "https://hooks.slack.com/services/T222/B222/second-path-token" || urls[1] != "https://hooks.slack.com/services/T111/B111/first-path-token" {
		t.Fatalf("restored reordered path-token URLs = %#v", urls)
	}
}

func TestRedactedSourceDetailsFailClosedForMalformedAddressAndPreserveHostPort(t *testing.T) {
	malformed := redactedSourceDetails(config.RuntimeConfig{
		HTTP:  config.RuntimeHTTPSourceConfig{URL: "http-user:http-password@config.example.test/live?token=query-secret"},
		Redis: config.RuntimeRedisSourceConfig{Addr: "redis-user:redis-password@redis.example.test:6379?token=query-secret"},
	})
	if got := malformed["http"].(map[string]any)["url"]; got != "" {
		t.Fatalf("malformed HTTP URL was returned as %q", got)
	}
	if got := malformed["redis"].(map[string]any)["addr"]; got != "" {
		t.Fatalf("malformed Redis address was returned as %q", got)
	}
	userinfo := redactedSourceDetails(config.RuntimeConfig{
		Redis: config.RuntimeRedisSourceConfig{Addr: "redis-user@redis.example.test:6379"},
	})
	if got := userinfo["redis"].(map[string]any)["addr"]; got != "" {
		t.Fatalf("credential-like Redis address was returned as %q", got)
	}

	plain := redactedSourceDetails(config.RuntimeConfig{Redis: config.RuntimeRedisSourceConfig{Addr: "127.0.0.1:6379"}})
	if got := plain["redis"].(map[string]any)["addr"]; got != "127.0.0.1:6379" {
		t.Fatalf("plain Redis host:port = %q, want preserved address", got)
	}
}

func TestHandleConfigDocumentRedactsRedisAddressCredentials(t *testing.T) {
	h, server := newTestHandlers(t)
	cfg := config.Defaults()
	cfg.Runtime.Source = "redis"
	//nolint:gosec // Synthetic URL credentials verify the management response boundary.
	cfg.Runtime.Redis.Addr = "redis://redis-user:redis-password@redis.example.test:6379?token=redis-secret#fragment"
	cfg.Runtime.Redis.Username = "liveforge"
	server.UpdateConfig(cfg)
	manager, err := configruntime.NewManager(configruntime.Options{Source: testConfigSource{}, Initial: cfg})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	server.SetConfigManager(manager)

	w := httptest.NewRecorder()
	h.handleConfigDocument(w, httptest.NewRequest(http.MethodGet, "/api/v1/server/config/document", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	data := decodeAPIData(t, w.Body.Bytes())
	var response struct {
		SourceDetails map[string]any `json:"source_details"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		t.Fatal(err)
	}
	redisDetails := response.SourceDetails["redis"].(map[string]any)
	if got := redisDetails["addr"]; got != "redis://redis.example.test:6379" {
		t.Fatalf("redacted Redis address = %q", got)
	}
	if got := redisDetails["username"]; got != "liveforge" {
		t.Fatalf("Redis ACL identity = %q", got)
	}
	for _, secret := range []string{"redis-user", "redis-password", "redis-secret", "token=", "fragment"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatalf("config document response leaked %q: %s", secret, w.Body.String())
		}
	}
}

func TestRedactedConfigDocumentRemovesURLCredentialsAndRestoresOnApply(t *testing.T) {
	//nolint:gosec // Intentional fake URL credentials verify redaction.
	const sourceDocument = `runtime:
  source: https
  http:
    url: https://user:password@config.example.test/live.yaml?token=source-secret
`
	redacted, err := redactedConfigDocument([]byte(sourceDocument))
	if err != nil {
		t.Fatal(err)
	}
	redactedText := string(redacted)
	for _, secret := range []string{"user", "password", "source-secret", "token="} {
		if strings.Contains(redactedText, secret) {
			t.Fatalf("redacted document leaked %q: %s", secret, redactedText)
		}
	}
	if !strings.Contains(redactedText, "REDACTED") {
		t.Fatalf("redacted URL did not contain an explicit marker: %s", redactedText)
	}

	current := config.Defaults()
	current.Runtime.Source = "https"
	current.Runtime.HTTP.URL = "https://user:password@config.example.test/live.yaml?token=source-secret"
	restored, err := preserveRedactedSecrets(redacted, current)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(restored), "https://user:password@config.example.test/live.yaml?token=source-secret") {
		t.Fatalf("restored document lost the original URL credentials: %q", restored)
	}
}

func TestRedactedConfigDocumentFailsClosedForHostlessURLAndPreservesPlainAddress(t *testing.T) {
	const sourceDocument = `custom_callback_url: callback-user:callback-password@callback.example.test/hook?token=query-secret
runtime:
  redis:
    addr: 127.0.0.1:6379
`
	redacted, err := redactedConfigDocument([]byte(sourceDocument))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"callback-user", "callback-password", "query-secret", "token="} {
		if strings.Contains(string(redacted), secret) {
			t.Fatalf("redacted document leaked %q: %s", secret, redacted)
		}
	}
	var document map[string]any
	if err := yaml.Unmarshal(redacted, &document); err != nil {
		t.Fatal(err)
	}
	if document["custom_callback_url"] != "[REDACTED]" {
		t.Fatalf("hostless callback URL = %#v, want opaque marker", document["custom_callback_url"])
	}
	if got := document["runtime"].(map[string]any)["redis"].(map[string]any)["addr"]; got != "127.0.0.1:6379" {
		t.Fatalf("plain Redis host:port = %q, want preserved address", got)
	}
}

func TestConfigMapRedactionFailsClosedForHostlessURLAndPreservesPlainAddress(t *testing.T) {
	document := map[string]any{
		"custom_callback_url": "callback-user:callback-password@callback.example.test/hook?token=query-secret",
		"runtime": map[string]any{
			"redis": map[string]any{"addr": "127.0.0.1:6379"},
		},
	}
	redactConfigValue(document)
	if document["custom_callback_url"] != "[REDACTED]" {
		t.Fatalf("hostless callback URL = %#v, want opaque marker", document["custom_callback_url"])
	}
	if got := document["runtime"].(map[string]any)["redis"].(map[string]any)["addr"]; got != "127.0.0.1:6379" {
		t.Fatalf("plain Redis host:port = %q, want preserved address", got)
	}
}

func TestConfigRedactionPreservesOnlyBareIPOrValidatedHostPortAddresses(t *testing.T) {
	const sourceDocument = `ipv4_address: 239.0.0.1
ipv6_address: "ff15::1"
hostname_address: relay.example.test
credential_address: relay-user@relay.example.test
path_address: /var/run/relay.sock
query_address: 239.0.0.1?token=query-secret
fragment_address: 239.0.0.1#fragment-secret
malformed_address: "[invalid"
redis_address: 127.0.0.1:6379
rtsp:
  multicast:
    address: 239.0.0.1
`

	assertAddresses := func(t *testing.T, document map[string]any) {
		t.Helper()
		if document["ipv4_address"] != "239.0.0.1" {
			t.Fatalf("bare IPv4 address = %#v, want preserved", document["ipv4_address"])
		}
		if document["ipv6_address"] != "ff15::1" {
			t.Fatalf("bare IPv6 address = %#v, want preserved", document["ipv6_address"])
		}
		if document["redis_address"] != "127.0.0.1:6379" {
			t.Fatalf("validated host:port = %#v, want preserved", document["redis_address"])
		}
		multicast := document["rtsp"].(map[string]any)["multicast"].(map[string]any)
		if multicast["address"] != "239.0.0.1" {
			t.Fatalf("RTSP multicast address = %#v, want preserved", multicast["address"])
		}
		for _, key := range []string{
			"hostname_address", "credential_address", "path_address", "query_address",
			"fragment_address", "malformed_address",
		} {
			if document[key] != "[REDACTED]" {
				t.Fatalf("unsafe %s = %#v, want opaque marker", key, document[key])
			}
		}
	}

	t.Run("YAML document", func(t *testing.T) {
		redacted, err := redactedConfigDocument([]byte(sourceDocument))
		if err != nil {
			t.Fatal(err)
		}
		var document map[string]any
		if err := yaml.Unmarshal(redacted, &document); err != nil {
			t.Fatal(err)
		}
		assertAddresses(t, document)
	})

	t.Run("decoded map", func(t *testing.T) {
		var document map[string]any
		if err := yaml.Unmarshal([]byte(sourceDocument), &document); err != nil {
			t.Fatal(err)
		}
		redactConfigValue(document)
		assertAddresses(t, document)
	})
}

func TestConfigRedactionPreservesSecretContainerShapeAndRedactsNestedValues(t *testing.T) {
	//nolint:gosec // Intentional fake credentials verify the management redaction boundary.
	const sourceDocument = `api:
  auth:
    tokens:
      - name: viewer
        token: viewer-secret
        role: viewer
notify:
  http:
    endpoints:
      - url: https://hook-user:hook-password@notify.example.test/live?token=query-secret#fragment-secret
        events: [publish]
        secret: webhook-secret
        retry: 2
        timeout: 3s
`
	redacted, err := redactedConfigDocument([]byte(sourceDocument))
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := yaml.Unmarshal(redacted, &document); err != nil {
		t.Fatal(err)
	}
	tokens, ok := document["api"].(map[string]any)["auth"].(map[string]any)["tokens"].([]any)
	if !ok || len(tokens) != 1 {
		t.Fatalf("tokens structure = %#v, want one-item sequence", document["api"])
	}
	token := tokens[0].(map[string]any)
	if token["name"] != "viewer" || token["role"] != "[REDACTED]" || token["token"] != "[REDACTED]" {
		t.Fatalf("redacted token = %#v", token)
	}
	endpoints, ok := document["notify"].(map[string]any)["http"].(map[string]any)["endpoints"].([]any)
	if !ok || len(endpoints) != 1 {
		t.Fatalf("endpoints structure = %#v, want one-item sequence", document["notify"])
	}
	endpoint := endpoints[0].(map[string]any)
	if endpoint["secret"] != "[REDACTED]" || endpoint["events"].([]any)[0] != "publish" {
		t.Fatalf("redacted endpoint = %#v", endpoint)
	}
	for _, leaked := range []string{"hook-user", "hook-password", "query-secret", "fragment-secret", "webhook-secret", "viewer-secret"} {
		if strings.Contains(string(redacted), leaked) {
			t.Fatalf("redacted document leaked %q: %s", leaked, redacted)
		}
	}
}

func TestRedactedConfigDocumentRedactsOpaqueSensitiveContainers(t *testing.T) {
	// #nosec G101 -- synthetic credential-like URLs exercise redaction without real secrets.
	const sourceDocument = `custom_credentials:
  name: primary
  value: mapping-secret
  nested:
    id: nested
    material: nested-secret
custom_private_keys:
  - name: first
    material: item-secret
  - raw-sequence-secret
  - [nested-sequence-secret]
`
	redacted, err := redactedConfigDocument([]byte(sourceDocument))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"mapping-secret", "nested-secret", "item-secret", "raw-sequence-secret", "nested-sequence-secret"} {
		if strings.Contains(string(redacted), secret) {
			t.Fatalf("redacted YAML leaked %q: %s", secret, redacted)
		}
	}
	var document map[string]any
	if err := yaml.Unmarshal(redacted, &document); err != nil {
		t.Fatal(err)
	}
	assertOpaqueSensitiveContainersRedacted(t, document)
}

func TestConfigMapRedactionRedactsOpaqueSensitiveContainers(t *testing.T) {
	const sourceDocument = `custom_credentials:
  name: primary
  value: mapping-secret
  nested:
    id: nested
    material: nested-secret
custom_private_keys:
  - name: first
    material: item-secret
  - raw-sequence-secret
  - [nested-sequence-secret]
`
	var document map[string]any
	if err := yaml.Unmarshal([]byte(sourceDocument), &document); err != nil {
		t.Fatal(err)
	}
	redactConfigValue(document)
	assertOpaqueSensitiveContainersRedacted(t, document)
}

func TestOpaqueSensitiveContainerRedactionKeepsStructuredURLValuesOpaque(t *testing.T) {
	// #nosec G101 -- synthetic credential-like URLs exercise redaction without real secrets.
	const sourceDocument = `custom_credentials:
  name: primary
  callback_url:
    name: mapping
    neutral: mapping-secret
    address:
      id: nested-address
      host: address-secret
  callback_urls:
    - name: sequence-entry
      neutral: sequence-secret
      endpoint:
        channel_id: nested-endpoint
        payload: endpoint-secret
    - scalar-sequence-secret
  public_url: https://url-user:url-password@public.example.test/hook?token=url-secret
  public_urls:
    - https://list-user:list-password@list.example.test/hook?token=list-secret
`

	t.Run("YAML document", func(t *testing.T) {
		redacted, err := redactedConfigDocument([]byte(sourceDocument))
		if err != nil {
			t.Fatal(err)
		}
		assertNoStructuredURLSecretLeak(t, string(redacted))
		var document map[string]any
		if err := yaml.Unmarshal(redacted, &document); err != nil {
			t.Fatal(err)
		}
		assertStructuredURLValuesOpaque(t, document)
	})

	t.Run("decoded map", func(t *testing.T) {
		var document map[string]any
		if err := yaml.Unmarshal([]byte(sourceDocument), &document); err != nil {
			t.Fatal(err)
		}
		redactConfigValue(document)
		encoded, err := yaml.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}
		assertNoStructuredURLSecretLeak(t, string(encoded))
		assertStructuredURLValuesOpaque(t, document)
	})
}

func TestPreserveRedactedSecretsRestoresOpaqueSensitiveItemsByIdentity(t *testing.T) {
	const currentDocument = `custom_private_keys:
  - {name: first, material: first-secret}
  - {name: second, material: second-secret}
`
	const candidateDocument = `custom_private_keys:
  - {name: second, material: "[REDACTED]"}
  - {name: first, material: "[REDACTED]"}
`
	restored, err := preserveRedactedSecretsWithDocument([]byte(candidateDocument), config.Defaults(), []byte(currentDocument))
	if err != nil {
		t.Fatal(err)
	}
	text := string(restored)
	if !strings.Contains(text, "first-secret") || !strings.Contains(text, "second-secret") || strings.Contains(text, "[REDACTED]") {
		t.Fatalf("opaque structured secrets were not restored by identity: %s", restored)
	}
}

func TestConfigMapRedactionPreservesNamedTokenAndEndpointCollections(t *testing.T) {
	current := config.Defaults()
	current.API.Auth.Tokens = []config.APIAuthToken{{Name: "viewer", Token: "viewer-secret", Role: "viewer"}}
	current.Notify.HTTP.Endpoints = []config.NotifyEndpointConfig{{
		URL: "https://notify.example.test/live?token=query-secret", Events: []string{"publish"}, Secret: "webhook-secret",
	}}

	redacted := configMapFromConfig(current)
	tokens, ok := redacted["api"].(map[string]any)["auth"].(map[string]any)["tokens"].([]any)
	if !ok || len(tokens) != 1 {
		t.Fatalf("tokens structure = %#v", redacted["api"])
	}
	if token := tokens[0].(map[string]any); token["name"] != "viewer" || token["token"] != "[REDACTED]" {
		t.Fatalf("redacted token = %#v", token)
	}
	endpoints, ok := redacted["notify"].(map[string]any)["http"].(map[string]any)["endpoints"].([]any)
	if !ok || len(endpoints) != 1 {
		t.Fatalf("endpoints structure = %#v", redacted["notify"])
	}
	if endpoint := endpoints[0].(map[string]any); endpoint["secret"] != "[REDACTED]" || strings.Contains(endpoint["url"].(string), "query-secret") {
		t.Fatalf("redacted endpoint = %#v", endpoint)
	}
}

func TestPreserveRedactedSecretsMatchesReorderedCollectionsByStableIdentity(t *testing.T) {
	//nolint:gosec // Intentional fake credentials verify identity-based restoration.
	const currentDocument = `api:
  auth:
    tokens:
      - {name: alpha, token: alpha-secret, role: viewer}
      - {name: beta, token: beta-secret, role: operator}
webrtc:
  ice_servers:
    - {urls: ["turn:one.example.test"], username: one, credential: ice-one}
    - {urls: ["turn:two.example.test"], username: two, credential: ice-two}
notify:
  http:
    endpoints:
      - {url: "https://one.example.test/hook?token=one-query", events: [publish], secret: hook-one, retry: 1, timeout: 1s}
      - {url: "https://two.example.test/hook?token=two-query", events: [unpublish], secret: hook-two, retry: 2, timeout: 2s}
`
	redacted, err := redactedConfigDocument([]byte(currentDocument))
	if err != nil {
		t.Fatal(err)
	}
	var candidate map[string]any
	if unmarshalErr := yaml.Unmarshal(redacted, &candidate); unmarshalErr != nil {
		t.Fatal(unmarshalErr)
	}
	reverseConfigSequence(t, candidate, "api", "auth", "tokens")
	reverseConfigSequence(t, candidate, "webrtc", "ice_servers")
	reverseConfigSequence(t, candidate, "notify", "http", "endpoints")
	candidateDocument, err := yaml.Marshal(candidate)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := preserveRedactedSecretsWithDocument(candidateDocument, config.Defaults(), []byte(currentDocument))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := yaml.Unmarshal(restored, &got); err != nil {
		t.Fatal(err)
	}
	assertConfigSecretByIdentity(t, got, []string{"api", "auth", "tokens"}, "name", "beta", "token", "beta-secret")
	assertConfigSecretByIdentity(t, got, []string{"webrtc", "ice_servers"}, "username", "two", "credential", "ice-two")
	assertConfigSecretByIdentity(t, got, []string{"notify", "http", "endpoints"}, "events", "unpublish", "secret", "hook-two")
}

func TestPreserveRedactedSecretsDoesNotTransplantDeletedSecretIntoInsertedItem(t *testing.T) {
	const currentDocument = `api:
  auth:
    tokens:
      - {name: keep, token: keep-secret, role: viewer}
      - {name: delete, token: delete-secret, role: viewer}
`
	const candidateDocument = `api:
  auth:
    tokens:
      - {name: inserted, token: inserted-secret, role: operator}
      - {name: keep, token: "[REDACTED]", role: viewer}
`
	restored, err := preserveRedactedSecretsWithDocument([]byte(candidateDocument), config.Defaults(), []byte(currentDocument))
	if err != nil {
		t.Fatal(err)
	}
	text := string(restored)
	if !strings.Contains(text, "inserted-secret") || !strings.Contains(text, "keep-secret") || strings.Contains(text, "delete-secret") {
		t.Fatalf("insert/delete restoration crossed identities: %s", text)
	}
}

func TestPreserveRedactedSecretsRejectsRenamedSingletonStructuredItem(t *testing.T) {
	const currentDocument = `api:
  auth:
    tokens:
      - {name: original, token: original-secret, role: viewer}
`
	const candidateDocument = `api:
  auth:
    tokens:
      - {name: renamed, token: "[REDACTED]", role: viewer}
`
	if _, err := preserveRedactedSecretsWithDocument([]byte(candidateDocument), config.Defaults(), []byte(currentDocument)); err == nil {
		t.Fatal("renamed singleton token received the original item's secret")
	}
}

func TestPreserveRedactedURLKeepsEditedLocationAndRestoresOnlySecretComponents(t *testing.T) {
	//nolint:gosec // Intentional fake URL credentials verify component restoration.
	const currentDocument = `runtime:
  source: https
  http:
    url: https://source-user:source-password@old.example.test/old.yaml?token=source-secret#source-fragment
`
	redacted, err := redactedConfigDocument([]byte(currentDocument))
	if err != nil {
		t.Fatal(err)
	}
	var edited map[string]any
	if unmarshalErr := yaml.Unmarshal(redacted, &edited); unmarshalErr != nil {
		t.Fatal(unmarshalErr)
	}
	edited["runtime"].(map[string]any)["http"].(map[string]any)["url"] = "https://REDACTED@new.example.test/new.yaml?__liveforge_redacted__=1"
	editedDocument, err := yaml.Marshal(edited)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := preserveRedactedSecretsWithDocument(editedDocument, config.Defaults(), []byte(currentDocument))
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"https://", "source-user", ":", "source-password", "@new.example.test/new.yaml",
		"?token=", "source-secret", "#", "source-fragment",
	}, "")
	if !strings.Contains(string(restored), want) {
		t.Fatalf("restored URL = %s, want edited location with original secret components %q", restored, want)
	}
}

func TestPreserveRedactedURLRestoresOpaqueScalarMarker(t *testing.T) {
	const currentDocument = `custom_callback_url: callback-user:callback-password@callback.example.test/hook?token=query-secret
`
	const candidateDocument = `custom_callback_url: "[REDACTED]"
`
	restored, err := preserveRedactedSecretsWithDocument([]byte(candidateDocument), config.Defaults(), []byte(currentDocument))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(restored), "callback-user:callback-password@callback.example.test/hook?token=query-secret") {
		t.Fatalf("opaque URL marker was persisted instead of restored: %s", restored)
	}
}

func TestPreserveRedactedURLRestoresOpaqueTURNURIQuery(t *testing.T) {
	const currentDocument = `webrtc:
  ice_servers:
    - urls: ["turn:relay.example.test:3478?transport=udp&token=turn-secret"]
      username: relay
      credential: relay-secret
`
	redacted, err := redactedConfigDocument([]byte(currentDocument))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(redacted), "turn-secret") {
		t.Fatalf("redacted TURN URI leaked its query: %s", redacted)
	}
	restored, err := preserveRedactedSecretsWithDocument(redacted, config.Defaults(), []byte(currentDocument))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(restored), "turn:relay.example.test:3478?transport=udp&token=turn-secret") {
		t.Fatalf("TURN URI query was not restored: %s", restored)
	}
}

func TestPreserveRedactedURLRejectsMarkedShapeMismatch(t *testing.T) {
	tests := []struct {
		name      string
		current   string
		candidate string
	}{
		{
			name:      "scalar original and sequence candidate",
			current:   "custom_callback_url: https://user:password@scalar.example.test/hook?token=secret\n",
			candidate: "custom_callback_url:\n  - https://REDACTED@scalar.example.test/hook?__liveforge_redacted__=1\n",
		},
		{
			name:      "scalar original and mapping candidate",
			current:   "custom_callback_url: https://user:password@scalar.example.test/hook?token=secret\n",
			candidate: "custom_callback_url:\n  primary_url: https://REDACTED@scalar.example.test/hook?__liveforge_redacted__=1\n",
		},
		{
			name:      "sequence original and scalar candidate",
			current:   "custom_callback_url:\n  - https://user:password@sequence.example.test/hook?token=secret\n",
			candidate: "custom_callback_url: \"[REDACTED]\"\n",
		},
		{
			name:      "sequence original and mapping candidate",
			current:   "custom_callback_url:\n  - https://user:password@sequence.example.test/hook?token=secret\n",
			candidate: "custom_callback_url:\n  primary_url: https://REDACTED@sequence.example.test/hook?__liveforge_redacted__=1\n",
		},
		{
			name:      "mapping original and scalar candidate",
			current:   "custom_callback_url:\n  primary_url: https://user:password@mapping.example.test/hook?token=secret\n",
			candidate: "custom_callback_url: \"[REDACTED]\"\n",
		},
		{
			name:      "mapping original and sequence candidate",
			current:   "custom_callback_url:\n  primary_url: https://user:password@mapping.example.test/hook?token=secret\n",
			candidate: "custom_callback_url:\n  - https://REDACTED@mapping.example.test/hook?__liveforge_redacted__=1\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			restored, err := preserveRedactedSecretsWithDocument([]byte(test.candidate), config.Defaults(), []byte(test.current))
			if err == nil {
				t.Fatalf("marked URL shape mismatch was accepted and produced: %s", restored)
			}
			if restored != nil {
				t.Fatalf("failed restoration returned a document containing placeholders: %s", restored)
			}
		})
	}
}

func TestPreserveRedactedURLSequenceMatchesReorderedPublicIdentity(t *testing.T) {
	const currentDocument = `custom_callback_urls:
  - https://first-user:first-password@first.example.test/hook?token=first-secret
  - https://second-user:second-password@second.example.test/hook?token=second-secret
`
	redacted, err := redactedConfigDocument([]byte(currentDocument))
	if err != nil {
		t.Fatal(err)
	}
	var candidate map[string]any
	if unmarshalErr := yaml.Unmarshal(redacted, &candidate); unmarshalErr != nil {
		t.Fatal(unmarshalErr)
	}
	reverseConfigSequence(t, candidate, "custom_callback_urls")
	candidateDocument, err := yaml.Marshal(candidate)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := preserveRedactedSecretsWithDocument(candidateDocument, config.Defaults(), []byte(currentDocument))
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := yaml.Unmarshal(restored, &document); err != nil {
		t.Fatal(err)
	}
	urls := document["custom_callback_urls"].([]any)
	wantFirst := "https://second-user:second-password@second.example.test/hook?token=second-secret" // #nosec G101 -- synthetic redaction fixture.
	wantSecond := "https://first-user:first-password@first.example.test/hook?token=first-secret"    // #nosec G101 -- synthetic redaction fixture.
	if len(urls) != 2 || urls[0] != wantFirst || urls[1] != wantSecond {
		t.Fatalf("restored reordered URLs = %#v, want [%q %q]", urls, wantFirst, wantSecond)
	}
}

func TestPreserveRedactedUnmappedURLSequenceUsesStableValueIdentity(t *testing.T) {
	// #nosec G101 -- synthetic credential-like URLs exercise redaction without real secrets.
	const currentDocument = `mirrors:
  - https://first-user:first-password@hooks.slack.com/services/T111/B111/first-path-token?token=first-query#first-fragment
  - https://second-user:second-password@hooks.slack.com/services/T222/B222/second-path-token?token=second-query#second-fragment
`
	redacted, err := redactedConfigDocument([]byte(currentDocument))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"first-user", "first-password", "T111", "B111", "first-path-token", "first-query", "second-user", "second-password", "T222", "B222", "second-path-token", "second-query"} {
		if strings.Contains(string(redacted), secret) {
			t.Errorf("redacted unmapped URL sequence leaked %q: %s", secret, redacted)
		}
	}

	t.Run("reordered", func(t *testing.T) {
		var candidate map[string]any
		if err := yaml.Unmarshal(redacted, &candidate); err != nil {
			t.Fatal(err)
		}
		reverseConfigSequence(t, candidate, "mirrors")
		candidateDocument, err := yaml.Marshal(candidate)
		if err != nil {
			t.Fatal(err)
		}
		restored, err := preserveRedactedSecretsWithDocument(candidateDocument, config.Defaults(), []byte(currentDocument))
		if err != nil {
			t.Fatal(err)
		}
		var document map[string]any
		if err := yaml.Unmarshal(restored, &document); err != nil {
			t.Fatal(err)
		}
		urls := document["mirrors"].([]any)
		wantFirst := "https://second-user:second-password@hooks.slack.com/services/T222/B222/second-path-token?token=second-query#second-fragment" // #nosec G101 -- synthetic redaction fixture.
		wantSecond := "https://first-user:first-password@hooks.slack.com/services/T111/B111/first-path-token?token=first-query#first-fragment"     // #nosec G101 -- synthetic redaction fixture.
		if len(urls) != 2 || urls[0] != wantFirst || urls[1] != wantSecond {
			t.Fatalf("restored reordered unmapped URLs = %#v, want [%q %q]", urls, wantFirst, wantSecond)
		}
	})

	t.Run("edited identity", func(t *testing.T) {
		var candidate map[string]any
		if err := yaml.Unmarshal(redacted, &candidate); err != nil {
			t.Fatal(err)
		}
		urls := candidate["mirrors"].([]any)
		urls[0] = strings.Replace(urls[0].(string), "hooks.slack.com", "edited.example.test", 1)
		candidateDocument, err := yaml.Marshal(candidate)
		if err != nil {
			t.Fatal(err)
		}
		if restored, err := preserveRedactedSecretsWithDocument(candidateDocument, config.Defaults(), []byte(currentDocument)); err == nil {
			t.Fatalf("edited unmapped URL identity received a source credential: %s", restored)
		}
	})

	t.Run("ambiguous identity", func(t *testing.T) {
		// #nosec G101 -- synthetic credential-like URLs exercise ambiguous restoration handling.
		const ambiguousDocument = `mirrors:
  - https://first-user:first-password@hooks.slack.com/services/SHARED/PATH/token?token=first-query
  - https://second-user:second-password@hooks.slack.com/services/SHARED/PATH/token?token=second-query
`
		ambiguous, err := redactedConfigDocument([]byte(ambiguousDocument))
		if err != nil {
			t.Fatal(err)
		}
		if restored, err := preserveRedactedSecretsWithDocument(ambiguous, config.Defaults(), []byte(ambiguousDocument)); err == nil {
			t.Fatalf("ambiguous unmapped URL identities were restored by position: %s", restored)
		}
	})
}

func TestPreserveRedactedURLRoundTripsLiteralPathMarker(t *testing.T) {
	//nolint:gosec // Intentional fake URL credentials verify placeholder restoration.
	const sourceURL = "https://source-user:source-password@hooks.slack.com/__liveforge_redacted_path__/0123456789abcdef0123456789abcdef?token=source-query#source-fragment"
	const redactedURL = "https://REDACTED@hooks.slack.com/__liveforge_redacted_path__/3b08eb10aa25a39ac0cf6bf776391a6b?__liveforge_redacted__=1"
	const currentDocument = "primary: " + sourceURL + "\n"
	redacted, err := redactedConfigDocument([]byte(currentDocument))
	if err != nil {
		t.Fatal(err)
	}
	var candidate map[string]any
	if unmarshalErr := yaml.Unmarshal(redacted, &candidate); unmarshalErr != nil {
		t.Fatal(unmarshalErr)
	}
	if candidate["primary"] != redactedURL {
		t.Fatalf("redacted URL = %q, want source-path digest %q", candidate["primary"], redactedURL)
	}

	restored, err := preserveRedactedSecretsWithDocument(redacted, config.Defaults(), []byte(currentDocument))
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := yaml.Unmarshal(restored, &document); err != nil {
		t.Fatal(err)
	}
	if document["primary"] != sourceURL {
		t.Fatalf("restored URL = %q, want exact source URL %q", document["primary"], sourceURL)
	}
}

func TestPreserveRedactedURLRejectsLiteralMarkerCandidateWithoutMatchingSourceDigest(t *testing.T) {
	//nolint:gosec // Intentional fake URL credentials verify fail-closed restoration.
	const currentDocument = "primary: https://source-user:source-password@hooks.slack.com/__liveforge_redacted_path__/0123456789abcdef0123456789abcdef?token=source-query#source-fragment\n"
	const candidateDocument = "primary: https://hooks.slack.com/__liveforge_redacted_path__/0123456789abcdef0123456789abcdef\n"
	restored, err := preserveRedactedSecretsWithDocument([]byte(candidateDocument), config.Defaults(), []byte(currentDocument))
	if err == nil {
		t.Fatalf("marker-looking candidate bypassed source-path identity matching: %s", restored)
	}
	if restored != nil {
		t.Fatalf("failed restoration returned a document containing source credentials: %s", restored)
	}
}

func TestPreserveRedactedSecretsRejectsAmbiguousCollectionIdentity(t *testing.T) {
	const currentDocument = `notify:
  http:
    endpoints:
      - {url: "https://one.example.test/hook?token=one", events: [publish], secret: one, retry: 1, timeout: 1s}
      - {url: "https://two.example.test/hook?token=two", events: [publish], secret: two, retry: 1, timeout: 1s}
`
	redacted, err := redactedConfigDocument([]byte(currentDocument))
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.ReplaceAll(string(redacted), "one.example.test", "edited-one.example.test")
	edited = strings.ReplaceAll(edited, "two.example.test", "edited-two.example.test")
	if _, err := preserveRedactedSecretsWithDocument([]byte(edited), config.Defaults(), []byte(currentDocument)); err == nil {
		t.Fatal("ambiguous endpoint identity was accepted")
	}
}

func reverseConfigSequence(t *testing.T, document map[string]any, path ...string) {
	t.Helper()
	var current any = document
	for _, key := range path {
		current = current.(map[string]any)[key]
	}
	items := current.([]any)
	for left, right := 0, len(items)-1; left < right; left, right = left+1, right-1 {
		items[left], items[right] = items[right], items[left]
	}
}

func assertConfigSecretByIdentity(t *testing.T, document map[string]any, path []string, identityKey, identityValue, secretKey, secretValue string) {
	t.Helper()
	var current any = document
	for _, key := range path {
		current = current.(map[string]any)[key]
	}
	for _, item := range current.([]any) {
		entry := item.(map[string]any)
		matches := entry[identityKey] == identityValue
		if values, ok := entry[identityKey].([]any); ok {
			matches = len(values) == 1 && values[0] == identityValue
		}
		if matches {
			if entry[secretKey] != secretValue {
				t.Fatalf("%s=%v for %s=%v, want %q", secretKey, entry[secretKey], identityKey, entry[identityKey], secretValue)
			}
			return
		}
	}
	t.Fatalf("identity %s=%q not found at %v", identityKey, identityValue, path)
}

func assertOpaqueSensitiveContainersRedacted(t *testing.T, document map[string]any) {
	t.Helper()
	credentials := document["custom_credentials"].(map[string]any)
	if credentials["name"] != "primary" || credentials["value"] != "[REDACTED]" {
		t.Fatalf("redacted custom_credentials = %#v", credentials)
	}
	nested := credentials["nested"].(map[string]any)
	if nested["id"] != "nested" || nested["material"] != "[REDACTED]" {
		t.Fatalf("redacted nested credentials = %#v", nested)
	}
	keys := document["custom_private_keys"].([]any)
	first := keys[0].(map[string]any)
	if first["name"] != "first" || first["material"] != "[REDACTED]" {
		t.Fatalf("redacted structured private key = %#v", first)
	}
	if keys[1] != "[REDACTED]" || keys[2].([]any)[0] != "[REDACTED]" {
		t.Fatalf("redacted heterogeneous private keys = %#v", keys)
	}
}

func assertNoStructuredURLSecretLeak(t *testing.T, encoded string) {
	t.Helper()
	for _, secret := range []string{
		"mapping-secret", "address-secret", "sequence-secret", "endpoint-secret",
		"scalar-sequence-secret", "url-user", "url-password", "url-secret",
		"list-user", "list-password", "list-secret",
	} {
		if strings.Contains(encoded, secret) {
			t.Fatalf("structured URL redaction leaked %q: %s", secret, encoded)
		}
	}
}

func assertStructuredURLValuesOpaque(t *testing.T, document map[string]any) {
	t.Helper()
	credentials := document["custom_credentials"].(map[string]any)
	callback := credentials["callback_url"].(map[string]any)
	if callback["name"] != "mapping" || callback["neutral"] != "[REDACTED]" {
		t.Fatalf("structured callback_url = %#v", callback)
	}
	address := callback["address"].(map[string]any)
	if address["id"] != "nested-address" || address["host"] != "[REDACTED]" {
		t.Fatalf("structured address = %#v", address)
	}
	callbacks := credentials["callback_urls"].([]any)
	entry := callbacks[0].(map[string]any)
	if entry["name"] != "sequence-entry" || entry["neutral"] != "[REDACTED]" {
		t.Fatalf("structured callback_urls entry = %#v", entry)
	}
	endpoint := entry["endpoint"].(map[string]any)
	if endpoint["channel_id"] != "nested-endpoint" || endpoint["payload"] != "[REDACTED]" {
		t.Fatalf("structured endpoint = %#v", endpoint)
	}
	if callbacks[1] != "[REDACTED]" {
		t.Fatalf("heterogeneous scalar URL value = %#v", callbacks[1])
	}
	publicURL := credentials["public_url"].(string)
	if !strings.Contains(publicURL, "public.example.test") || strings.Contains(publicURL, "/hook") || !isRedactedConfigURL(publicURL) {
		t.Fatalf("scalar public_url lost safe identity: %q", publicURL)
	}
	publicURLs := credentials["public_urls"].([]any)
	if len(publicURLs) != 1 || !strings.Contains(publicURLs[0].(string), "list.example.test") || strings.Contains(publicURLs[0].(string), "/hook") || !isRedactedConfigURL(publicURLs[0].(string)) {
		t.Fatalf("scalar public_urls lost safe identity: %#v", publicURLs)
	}
}

type rawDocumentSource struct {
	document []byte
}

func (s *rawDocumentSource) Load(context.Context, configruntime.Version) (configruntime.Snapshot, error) {
	return configruntime.Snapshot{Data: append([]byte(nil), s.document...), Version: "raw-1"}, nil
}

func (s *rawDocumentSource) Close() error { return nil }

type errorConfigWriterSource struct{ err error }

func (s errorConfigWriterSource) Load(context.Context, configruntime.Version) (configruntime.Snapshot, error) {
	return configruntime.Snapshot{}, s.err
}

func (s errorConfigWriterSource) Write(context.Context, []byte) error { return s.err }
func (s errorConfigWriterSource) Close() error                        { return nil }
