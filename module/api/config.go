package api

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/im-pingo/liveforge/config"
	configruntime "github.com/im-pingo/liveforge/config/runtime"
	"gopkg.in/yaml.v3"
)

// Keep the runtime response independent from the process working directory.
// The repository copy is checked against this embedded asset in tests.
//
//go:embed configschema/config.schema.json
var embeddedConfigSchema []byte

type configDocumentResponse struct {
	Effective     map[string]any `json:"effective"`
	Desired       map[string]any `json:"desired"`
	EffectiveText string         `json:"effective_document"`
	DesiredText   string         `json:"desired_document"`
	SourceDetails map[string]any `json:"source_details"`
	Schema        map[string]any `json:"schema"`
	Writable      bool           `json:"writable"`
}

func (h *Handlers) handleConfigDocument(w http.ResponseWriter, r *http.Request) {
	manager := h.server.ConfigManager()
	if manager == nil {
		effectiveMap := configMapFromConfig(h.server.Config())
		writeJSON(w, http.StatusOK, configDocumentResponse{
			Effective:     effectiveMap,
			Desired:       effectiveMap,
			EffectiveText: configTextFromMap(effectiveMap),
			DesiredText:   configTextFromMap(effectiveMap),
			Schema:        configSchemaDescriptor(),
			SourceDetails: map[string]any{
				"kind": "bootstrap",
			},
		})
		return
	}
	snapshot := manager.Snapshot()
	effective := h.server.Config()
	desired := effective
	if snapshot != nil {
		if snapshot.Config != nil {
			effective = snapshot.Config
		}
		if snapshot.DesiredConfig != nil {
			desired = snapshot.DesiredConfig
		}
	}
	effectiveMap := configMapFromConfig(effective)
	desiredMap := configMapFromConfig(desired)
	desiredText := configTextFromMap(desiredMap)
	if snapshot != nil && len(snapshot.DesiredDocument) > 0 {
		if sourceMap, err := configMapFromDocument(snapshot.DesiredDocument); err == nil {
			desiredMap = sourceMap
		}
		if sourceText, err := redactedConfigDocument(snapshot.DesiredDocument); err == nil {
			desiredText = string(sourceText)
		}
	}
	writeJSON(w, http.StatusOK, configDocumentResponse{
		Effective:     effectiveMap,
		Desired:       desiredMap,
		EffectiveText: configTextFromMap(effectiveMap),
		DesiredText:   desiredText,
		SourceDetails: redactedSourceDetails(effective.Runtime),
		Schema:        configSchemaDescriptor(),
		Writable:      manager.SourceWritable(),
	})
}

func (h *Handlers) handleConfigSchema(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, configSchemaDescriptor())
}

func (h *Handlers) handleConfigValidate(w http.ResponseWriter, r *http.Request) {
	document, err := readConfigDocument(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	cfg, err := configruntime.ValidateDocument(document)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"valid":  true,
		"config": configMapFromConfig(cfg),
	})
}

func (h *Handlers) handleConfigApply(w http.ResponseWriter, r *http.Request) {
	manager := h.server.ConfigManager()
	if manager == nil {
		writeError(w, http.StatusServiceUnavailable, "runtime config manager unavailable")
		return
	}
	document, err := readConfigDocument(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	secretSource := h.server.Config()
	if snapshot := manager.Snapshot(); snapshot != nil {
		if snapshot.DesiredConfig != nil {
			secretSource = snapshot.DesiredConfig
		} else if snapshot.Config != nil {
			secretSource = snapshot.Config
		}
	}
	document, err = preserveRedactedSecrets(document, secretSource)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := manager.Write(r.Context(), document); err != nil {
		status := http.StatusServiceUnavailable
		if strings.Contains(err.Error(), "read-only") {
			status = http.StatusConflict
		} else if _, parseErr := configruntime.ValidateDocument(document); parseErr != nil {
			status = http.StatusBadRequest
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "written_and_refresh_scheduled"})
}

func readConfigDocument(r *http.Request) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r.Body, 4<<20+1))
	if err != nil {
		return nil, fmt.Errorf("read configuration document: %w", err)
	}
	if len(data) > 4<<20 {
		return nil, fmt.Errorf("configuration document exceeds 4 MiB")
	}
	if strings.Contains(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		var request struct {
			Document string `json:"document"`
		}
		if err := json.Unmarshal(data, &request); err != nil || strings.TrimSpace(request.Document) == "" {
			return nil, fmt.Errorf("JSON body must contain a non-empty document")
		}
		return []byte(request.Document), nil
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, fmt.Errorf("configuration document is empty")
	}
	return data, nil
}

func configMapFromConfig(cfg *config.Config) map[string]any {
	value := rawConfigMapFromConfig(cfg)
	redactConfigValue(value)
	return value
}

func configMapFromDocument(document []byte) (map[string]any, error) {
	var value map[string]any
	if err := yaml.Unmarshal(document, &value); err != nil {
		return nil, err
	}
	redactConfigValue(value)
	return value, nil
}

func rawConfigMapFromConfig(cfg *config.Config) map[string]any {
	if cfg == nil {
		return map[string]any{}
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return map[string]any{}
	}
	var value map[string]any
	if yaml.Unmarshal(data, &value) != nil {
		return map[string]any{}
	}
	return value
}

func configTextFromMap(value map[string]any) string {
	data, err := yaml.Marshal(value)
	if err != nil {
		return ""
	}
	return string(data)
}

func redactedConfigDocument(document []byte) ([]byte, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(document, &root); err != nil {
		return nil, err
	}
	redactYAMLNode(&root)
	return yaml.Marshal(&root)
}

func redactYAMLNode(node *yaml.Node) {
	if node == nil {
		return
	}
	switch node.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, child := range node.Content {
			redactYAMLNode(child)
		}
	case yaml.MappingNode:
		for index := 0; index+1 < len(node.Content); index += 2 {
			key, value := node.Content[index], node.Content[index+1]
			if key.Kind == yaml.ScalarNode && isSensitiveConfigKey(key.Value) && value.Kind == yaml.ScalarNode {
				value.Tag = "!!str"
				value.Style = yaml.DoubleQuotedStyle
				value.Value = "[REDACTED]"
				continue
			}
			redactYAMLNode(value)
		}
	}
}

func preserveRedactedSecrets(document []byte, current *config.Config) ([]byte, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(document, &root); err != nil {
		return nil, err
	}
	restoreRedactedYAMLSecrets(&root, rawConfigMapFromConfig(current))
	return yaml.Marshal(&root)
}

func restoreRedactedYAMLSecrets(node *yaml.Node, current any) {
	if node == nil {
		return
	}
	switch node.Kind {
	case yaml.DocumentNode:
		for _, child := range node.Content {
			restoreRedactedYAMLSecrets(child, current)
		}
	case yaml.MappingNode:
		original, _ := current.(map[string]any)
		for index := 0; index+1 < len(node.Content); index += 2 {
			key, value := node.Content[index], node.Content[index+1]
			if key.Kind != yaml.ScalarNode {
				continue
			}
			replacement, ok := original[key.Value]
			if isSensitiveConfigKey(key.Value) && isRedactedYAMLNode(value) && ok {
				replaceYAMLNode(value, replacement)
				continue
			}
			restoreRedactedYAMLSecrets(value, replacement)
		}
	case yaml.SequenceNode:
		original, _ := current.([]any)
		for index, child := range node.Content {
			var replacement any
			if index < len(original) {
				replacement = original[index]
			}
			restoreRedactedYAMLSecrets(child, replacement)
		}
	}
}

func isRedactedYAMLNode(node *yaml.Node) bool {
	if node == nil {
		return false
	}
	if node.Kind == yaml.ScalarNode {
		return node.Value == "[REDACTED]"
	}
	return node.Kind == yaml.SequenceNode && len(node.Content) == 1 && node.Content[0].Value == "REDACTED"
}

func replaceYAMLNode(node *yaml.Node, value any) {
	data, err := yaml.Marshal(value)
	if err != nil {
		return
	}
	var replacement yaml.Node
	if err := yaml.Unmarshal(data, &replacement); err != nil || len(replacement.Content) == 0 {
		return
	}
	*node = *replacement.Content[0]
}

func mergeRedactedSecrets(candidate, current any) {
	switch value := candidate.(type) {
	case map[string]any:
		original, _ := current.(map[string]any)
		for key, child := range value {
			if isSensitiveConfigKey(key) && child == "[REDACTED]" {
				if replacement, ok := original[key]; ok {
					value[key] = replacement
				}
				continue
			}
			mergeRedactedSecrets(child, original[key])
		}
	case []any:
		original, _ := current.([]any)
		for index, child := range value {
			var replacement any
			if index < len(original) {
				replacement = original[index]
			}
			mergeRedactedSecrets(child, replacement)
		}
	}
}

func redactConfigValue(value any) {
	switch current := value.(type) {
	case map[string]any:
		for key, child := range current {
			switch child.(type) {
			case map[string]any, []any:
				redactConfigValue(child)
				continue
			}
			if isSensitiveConfigKey(key) {
				current[key] = "[REDACTED]"
				continue
			}
			redactConfigValue(child)
		}
	case []any:
		for _, child := range current {
			redactConfigValue(child)
		}
	}
}

func isSensitiveConfigKey(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	for _, marker := range []string{"token", "password", "secret", "credential", "passphrase", "private_key"} {
		if strings.Contains(key, marker) {
			return true
		}
	}
	return false
}

func redactedSourceDetails(runtimeConfig config.RuntimeConfig) map[string]any {
	return map[string]any{
		"kind":   runtimeConfig.Source,
		"file":   map[string]any{"path": runtimeConfig.File.Path},
		"http":   map[string]any{"url": redactedSourceURL(runtimeConfig.HTTP.URL), "max_bytes": runtimeConfig.HTTP.MaxBytes},
		"consul": map[string]any{"address": redactedSourceURL(runtimeConfig.Consul.Address), "prefix": runtimeConfig.Consul.Prefix, "max_bytes": runtimeConfig.Consul.MaxBytes},
		"redis":  map[string]any{"addr": runtimeConfig.Redis.Addr, "username": runtimeConfig.Redis.Username, "db": runtimeConfig.Redis.DB, "prefix": runtimeConfig.Redis.Prefix, "hash": runtimeConfig.Redis.Hash, "version_key": runtimeConfig.Redis.VersionKey, "tls": runtimeConfig.Redis.TLS},
	}
}

func redactedSourceURL(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

func configSchemaDescriptor() map[string]any {
	var schema map[string]any
	if err := json.Unmarshal(embeddedConfigSchema, &schema); err != nil {
		return map[string]any{
			"$schema": "https://json-schema.org/draft/2020-12/schema",
			"title":   "LiveForge configuration",
			"error":   "embedded configuration schema is invalid",
		}
	}
	return schema
}
