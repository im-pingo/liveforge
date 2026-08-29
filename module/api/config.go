package api

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
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
	var sourceDocument []byte
	if snapshot := manager.Snapshot(); snapshot != nil {
		if snapshot.DesiredConfig != nil {
			secretSource = snapshot.DesiredConfig
		} else if snapshot.Config != nil {
			secretSource = snapshot.Config
		}
		sourceDocument = append([]byte(nil), snapshot.DesiredDocument...)
	}
	document, err = preserveRedactedSecretsWithDocument(document, secretSource, sourceDocument)
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
		writeError(w, status, configruntime.RedactError(err))
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
			if key.Kind != yaml.ScalarNode {
				redactYAMLNode(value)
				continue
			}
			if isSensitiveConfigKey(key.Value) && redactSensitiveYAMLNode(value) {
				continue
			}
			if isURLConfigKey(key.Value) {
				redactURLYAMLNode(value, isAddressConfigKey(key.Value))
				continue
			}
			redactYAMLNode(value)
		}
	}
}

func redactSensitiveYAMLNode(node *yaml.Node) bool {
	if node == nil {
		return false
	}
	redactOpaqueSensitiveYAMLNode(node)
	return true
}

func redactOpaqueSensitiveYAMLNode(node *yaml.Node) {
	if node == nil {
		return
	}
	switch node.Kind {
	case yaml.ScalarNode:
		node.Tag = "!!str"
		node.Style = yaml.DoubleQuotedStyle
		node.Value = "[REDACTED]"
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, child := range node.Content {
			redactOpaqueSensitiveYAMLNode(child)
		}
	case yaml.MappingNode:
		for index := 0; index+1 < len(node.Content); index += 2 {
			key, value := node.Content[index], node.Content[index+1]
			if key.Kind == yaml.ScalarNode {
				if isStableConfigIdentityKey(key.Value) && value.Kind == yaml.ScalarNode {
					continue
				}
				if isURLConfigKey(key.Value) && isScalarURLYAMLNode(value) {
					redactURLYAMLNode(value, isAddressConfigKey(key.Value))
					continue
				}
			}
			redactOpaqueSensitiveYAMLNode(value)
		}
	}
}

func isScalarURLYAMLNode(node *yaml.Node) bool {
	if node == nil {
		return false
	}
	if node.Kind == yaml.ScalarNode {
		return true
	}
	if node.Kind != yaml.SequenceNode {
		return false
	}
	for _, child := range node.Content {
		if child.Kind != yaml.ScalarNode {
			return false
		}
	}
	return true
}

func preserveRedactedSecrets(document []byte, current *config.Config) ([]byte, error) {
	return preserveRedactedSecretsWithDocument(document, current, nil)
}

func preserveRedactedSecretsWithDocument(document []byte, current *config.Config, currentDocument []byte) ([]byte, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(document, &root); err != nil {
		return nil, err
	}
	currentValues := rawConfigMapFromConfig(current)
	if len(currentDocument) > 0 {
		var sourceValues map[string]any
		if err := yaml.Unmarshal(currentDocument, &sourceValues); err == nil {
			mergeConfigMaps(currentValues, sourceValues)
		}
	}
	if err := restoreRedactedYAMLSecrets(&root, currentValues, nil); err != nil {
		return nil, err
	}
	return yaml.Marshal(&root)
}

func restoreRedactedYAMLSecrets(node *yaml.Node, current any, path []string) error {
	if node == nil {
		return nil
	}
	switch node.Kind {
	case yaml.DocumentNode:
		for _, child := range node.Content {
			if err := restoreRedactedYAMLSecrets(child, current, path); err != nil {
				return err
			}
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
			childPath := appendConfigPath(path, key.Value)
			if value.Kind == yaml.ScalarNode && value.Value == "[REDACTED]" && configPathContainsSensitiveKey(childPath) {
				if !ok || !isScalarConfigValue(replacement) {
					return fmt.Errorf("cannot restore redacted configuration value at %s", formatConfigPath(childPath))
				}
				replaceYAMLNode(value, replacement)
				continue
			}
			if isSensitiveConfigKey(key.Value) && isRedactedYAMLNode(value) && !ok {
				return fmt.Errorf("cannot restore redacted configuration value at %s", formatConfigPath(childPath))
			}
			if isURLConfigKey(key.Value) {
				if err := restoreRedactedURLYAMLNode(value, replacement, childPath); err != nil {
					return err
				}
				continue
			}
			if err := restoreRedactedYAMLSecrets(value, replacement, childPath); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		return restoreRedactedYAMLSequence(node, current, path)
	case yaml.ScalarNode:
		if !isRedactedConfigURL(node.Value) {
			return nil
		}
		source, ok := current.(string)
		if !ok {
			return fmt.Errorf("cannot restore redacted URL at %s", formatConfigPath(path))
		}
		restored, err := restoreRedactedConfigURL(node.Value, source)
		if err != nil {
			return fmt.Errorf("restore redacted URL at %s: %w", formatConfigPath(path), err)
		}
		replaceYAMLNode(node, restored)
	}
	return nil
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

func redactURLYAMLNode(node *yaml.Node, allowPlainAddress bool) {
	if node == nil {
		return
	}
	switch node.Kind {
	case yaml.ScalarNode:
		if redacted := redactConfigURL(node.Value, allowPlainAddress); redacted != node.Value {
			node.Tag = "!!str"
			node.Style = yaml.DoubleQuotedStyle
			node.Value = redacted
		}
	case yaml.SequenceNode:
		for _, child := range node.Content {
			redactURLYAMLNode(child, allowPlainAddress)
		}
	case yaml.DocumentNode, yaml.MappingNode:
		redactYAMLNode(node)
	}
}

func restoreRedactedURLYAMLNode(node *yaml.Node, current any, path []string) error {
	if node == nil {
		return nil
	}
	containsRedaction := yamlNodeContainsRedaction(node)
	switch value := current.(type) {
	case string:
		if containsRedaction && node.Kind != yaml.ScalarNode {
			return fmt.Errorf("cannot restore redacted URL at %s: value shape changed", formatConfigPath(path))
		}
		if node.Kind != yaml.ScalarNode {
			return nil
		}
		if node.Value == "[REDACTED]" {
			if value == "[REDACTED]" {
				return fmt.Errorf("cannot restore redacted URL at %s: source URL is unavailable", formatConfigPath(path))
			}
			replaceYAMLNode(node, value)
			return nil
		}
		if isRedactedConfigURL(node.Value) {
			restored, err := restoreRedactedConfigURL(node.Value, value)
			if err != nil {
				return fmt.Errorf("restore redacted URL at %s: %w", formatConfigPath(path), err)
			}
			replaceYAMLNode(node, restored)
		}
	case []any:
		if containsRedaction && node.Kind != yaml.SequenceNode {
			return fmt.Errorf("cannot restore redacted URL at %s: value shape changed", formatConfigPath(path))
		}
		if node.Kind != yaml.SequenceNode {
			return nil
		}
		return restoreRedactedYAMLSequence(node, value, path)
	case map[string]any:
		if containsRedaction && node.Kind != yaml.MappingNode {
			return fmt.Errorf("cannot restore redacted URL at %s: value shape changed", formatConfigPath(path))
		}
		return restoreRedactedYAMLSecrets(node, value, path)
	default:
		if containsRedaction {
			return fmt.Errorf("cannot restore redacted URL at %s", formatConfigPath(path))
		}
	}
	return nil
}

func restoreRedactedConfigURL(candidate, current string) (string, error) {
	if !isRedactedConfigURL(candidate) {
		return candidate, nil
	}
	candidateURL, err := url.Parse(strings.TrimSpace(candidate))
	if err != nil || !isValidConfigURL(candidateURL) {
		return "", fmt.Errorf("candidate URL is invalid")
	}
	currentURL, err := url.Parse(strings.TrimSpace(current))
	if err != nil || !isValidConfigURL(currentURL) {
		return "", fmt.Errorf("source URL is unavailable")
	}
	candidateURL.User = currentURL.User
	candidateURL.RawQuery = currentURL.RawQuery
	candidateURL.ForceQuery = currentURL.ForceQuery
	candidateURL.Fragment = currentURL.Fragment
	return candidateURL.String(), nil
}

func restoreRedactedYAMLSequence(node *yaml.Node, current any, path []string) error {
	original, _ := current.([]any)
	redactedIndexes := make([]int, 0, len(node.Content))
	for index, child := range node.Content {
		if yamlNodeContainsRedaction(child) {
			redactedIndexes = append(redactedIndexes, index)
		}
	}
	if len(redactedIndexes) == 0 {
		return nil
	}
	if len(node.Content) == 1 && len(original) == 1 {
		if child := node.Content[0]; child.Kind == yaml.ScalarNode && child.Value == "[REDACTED]" &&
			isImmediateSensitiveConfigPath(path) && isScalarConfigValue(original[0]) {
			replaceYAMLNode(child, original[0])
			return nil
		}
	}

	candidateValues := make([]any, len(node.Content))
	for index, child := range node.Content {
		if err := child.Decode(&candidateValues[index]); err != nil {
			return fmt.Errorf("decode configuration collection at %s[%d]: %w", formatConfigPath(path), index, err)
		}
	}
	candidateIdentities := make([][]string, len(candidateValues))
	for index, value := range candidateValues {
		candidateIdentities[index] = configStableIdentities(value)
	}
	originalIdentities := make([][]string, len(original))
	for index, value := range original {
		originalIdentities[index] = configStableIdentities(value)
	}
	used := make(map[int]struct{}, len(redactedIndexes))
	for _, candidateIndex := range redactedIndexes {
		originalIndex := uniqueConfigIdentityMatch(candidateIndex, candidateIdentities, originalIdentities, used)
		if originalIndex < 0 {
			return fmt.Errorf("cannot uniquely restore redacted configuration item at %s[%d]", formatConfigPath(path), candidateIndex)
		}
		used[originalIndex] = struct{}{}
		if err := restoreRedactedYAMLSecrets(node.Content[candidateIndex], original[originalIndex], appendConfigPath(path, fmt.Sprintf("[%d]", candidateIndex))); err != nil {
			return err
		}
	}
	return nil
}

func isImmediateSensitiveConfigPath(path []string) bool {
	if len(path) == 0 {
		return false
	}
	return isSensitiveConfigKey(path[len(path)-1])
}

func configPathContainsSensitiveKey(path []string) bool {
	for _, element := range path {
		if isSensitiveConfigKey(element) {
			return true
		}
	}
	return false
}

func isScalarConfigValue(value any) bool {
	switch value.(type) {
	case map[string]any, []any:
		return false
	default:
		return true
	}
}

func uniqueConfigIdentityMatch(candidateIndex int, candidates, originals [][]string, used map[int]struct{}) int {
	for _, identity := range candidates[candidateIndex] {
		candidateCount := 0
		for _, identities := range candidates {
			if containsConfigIdentity(identities, identity) {
				candidateCount++
			}
		}
		if candidateCount != 1 {
			continue
		}
		match := -1
		for originalIndex, identities := range originals {
			if _, exists := used[originalIndex]; exists || !containsConfigIdentity(identities, identity) {
				continue
			}
			if match >= 0 {
				match = -1
				break
			}
			match = originalIndex
		}
		if match >= 0 {
			return match
		}
	}
	return -1
}

func configStableIdentities(value any) []string {
	if text, ok := value.(string); ok {
		if identity := publicConfigURLIdentity(text); identity != "" {
			return []string{"url:" + identity}
		}
		return nil
	}
	mapping, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	identities := make([]string, 0, 5)
	for _, key := range []string{"id", "name", "username", "channel_id", "device_id"} {
		if identity := scalarConfigIdentity(mapping[key]); identity != "" {
			identities = append(identities, key+":"+identity)
		}
	}
	nonSecret := make(map[string]any)
	for key, child := range mapping {
		if isSensitiveConfigKey(key) || isURLConfigKey(key) || configValueContainsRedaction(child) {
			continue
		}
		nonSecret[key] = child
	}
	if len(nonSecret) > 0 {
		if encoded, err := json.Marshal(nonSecret); err == nil {
			identities = append(identities, "fields:"+string(encoded))
		}
	}
	for key, child := range mapping {
		if !isURLConfigKey(key) {
			continue
		}
		switch urls := child.(type) {
		case string:
			if identity := publicConfigURLIdentity(urls); identity != "" {
				identities = append(identities, key+":"+identity)
			}
		case []any:
			public := make([]string, 0, len(urls))
			for _, item := range urls {
				if text, ok := item.(string); ok {
					public = append(public, publicConfigURLIdentity(text))
				}
			}
			if encoded, err := json.Marshal(public); err == nil {
				identities = append(identities, key+":"+string(encoded))
			}
		}
	}
	return identities
}

func isStableConfigIdentityKey(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "id", "name", "username", "channel_id", "device_id":
		return true
	default:
		return false
	}
}

func publicConfigURLIdentity(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" {
		return strings.TrimSpace(raw)
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	return parsed.String()
}

func scalarConfigIdentity(value any) string {
	switch value := value.(type) {
	case string:
		return strings.TrimSpace(value)
	case fmt.Stringer:
		return value.String()
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return fmt.Sprint(value)
	default:
		return ""
	}
}

func containsConfigIdentity(identities []string, identity string) bool {
	for _, candidate := range identities {
		if candidate == identity {
			return true
		}
	}
	return false
}

func yamlNodeContainsRedaction(node *yaml.Node) bool {
	if node == nil {
		return false
	}
	if node.Kind == yaml.ScalarNode && (node.Value == "[REDACTED]" || isRedactedConfigURL(node.Value)) {
		return true
	}
	for _, child := range node.Content {
		if yamlNodeContainsRedaction(child) {
			return true
		}
	}
	return false
}

func configValueContainsRedaction(value any) bool {
	switch value := value.(type) {
	case string:
		return value == "[REDACTED]" || isRedactedConfigURL(value)
	case []any:
		for _, child := range value {
			if configValueContainsRedaction(child) {
				return true
			}
		}
	case map[string]any:
		for _, child := range value {
			if configValueContainsRedaction(child) {
				return true
			}
		}
	}
	return false
}

func appendConfigPath(path []string, element string) []string {
	appended := make([]string, len(path), len(path)+1)
	copy(appended, path)
	return append(appended, element)
}

func formatConfigPath(path []string) string {
	if len(path) == 0 {
		return "configuration"
	}
	return strings.Join(path, ".")
}

func mergeConfigMaps(dst, src map[string]any) {
	for key, sourceValue := range src {
		destinationValue := dst[key]
		sourceMap, sourceIsMap := sourceValue.(map[string]any)
		destinationMap, destinationIsMap := destinationValue.(map[string]any)
		if sourceIsMap && destinationIsMap {
			mergeConfigMaps(destinationMap, sourceMap)
			continue
		}
		dst[key] = sourceValue
	}
}

func redactConfigValue(value any) {
	switch current := value.(type) {
	case map[string]any:
		for key, child := range current {
			if isSensitiveConfigKey(key) {
				if redactSensitiveConfigValue(current, key, child) {
					continue
				}
			}
			if isURLConfigKey(key) {
				redactConfigURLValue(current, key, child)
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

func redactSensitiveConfigValue(parent map[string]any, key string, value any) bool {
	switch value.(type) {
	case map[string]any, []any:
		redactOpaqueSensitiveConfigValue(value)
	default:
		parent[key] = "[REDACTED]"
	}
	return true
}

func redactOpaqueSensitiveConfigValue(value any) {
	switch current := value.(type) {
	case map[string]any:
		for key, child := range current {
			if isStableConfigIdentityKey(key) && isScalarConfigValue(child) {
				continue
			}
			if isURLConfigKey(key) && isScalarConfigURLValue(child) {
				redactConfigURLValue(current, key, child)
				continue
			}
			switch child.(type) {
			case map[string]any, []any:
				redactOpaqueSensitiveConfigValue(child)
			default:
				current[key] = "[REDACTED]"
			}
		}
	case []any:
		for index, child := range current {
			switch child.(type) {
			case map[string]any, []any:
				redactOpaqueSensitiveConfigValue(child)
			default:
				current[index] = "[REDACTED]"
			}
		}
	}
}

func isScalarConfigURLValue(value any) bool {
	switch value := value.(type) {
	case string:
		return true
	case []any:
		for _, child := range value {
			if _, ok := child.(string); !ok {
				return false
			}
		}
		return true
	default:
		return false
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

const redactedURLQuery = "__liveforge_redacted__=1"

func isURLConfigKey(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	return key == "url" || key == "urls" || strings.Contains(key, "_url") ||
		strings.Contains(key, "uri") || strings.Contains(key, "endpoint") ||
		strings.Contains(key, "address") || key == "addr"
}

func isAddressConfigKey(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	return strings.Contains(key, "address") || key == "addr"
}

func redactConfigURLValue(parent map[string]any, key string, value any) {
	switch value := value.(type) {
	case string:
		parent[key] = redactConfigURL(value, isAddressConfigKey(key))
	case []any:
		for index, child := range value {
			if text, ok := child.(string); ok {
				value[index] = redactConfigURL(text, isAddressConfigKey(key))
				continue
			}
			redactConfigValue(child)
		}
	case map[string]any:
		redactConfigValue(value)
	}
}

func redactConfigURL(raw string, allowPlainAddress bool) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || !isValidConfigURL(parsed) {
		if allowPlainAddress && (net.ParseIP(trimmed) != nil || isPlainHostPort(trimmed)) {
			return trimmed
		}
		return "[REDACTED]"
	}
	if parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == "" {
		return trimmed
	}
	if parsed.User != nil {
		parsed.User = url.User("REDACTED")
	}
	parsed.RawQuery = redactedURLQuery
	parsed.ForceQuery = false
	parsed.Fragment = ""
	return parsed.String()
}

func isRedactedConfigURL(raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && isValidConfigURL(parsed) &&
		(parsed.RawQuery == redactedURLQuery || (parsed.User != nil && parsed.User.Username() == "REDACTED"))
}

func isValidConfigURL(parsed *url.URL) bool {
	if parsed == nil || parsed.Scheme == "" {
		return false
	}
	if parsed.Host != "" {
		return true
	}
	switch strings.ToLower(parsed.Scheme) {
	case "stun", "stuns", "turn", "turns":
		return parsed.Opaque != "" && !strings.ContainsAny(parsed.Opaque, "@/\\\r\n\t ")
	default:
		return false
	}
}

func redactedSourceDetails(runtimeConfig config.RuntimeConfig) map[string]any {
	return map[string]any{
		"kind":   runtimeConfig.Source,
		"file":   map[string]any{"path": runtimeConfig.File.Path},
		"http":   map[string]any{"url": redactedSourceURL(runtimeConfig.HTTP.URL), "max_bytes": runtimeConfig.HTTP.MaxBytes},
		"consul": map[string]any{"address": redactedSourceURL(runtimeConfig.Consul.Address), "prefix": runtimeConfig.Consul.Prefix, "max_bytes": runtimeConfig.Consul.MaxBytes},
		"redis":  map[string]any{"addr": redactedSourceAddress(runtimeConfig.Redis.Addr), "username": runtimeConfig.Redis.Username, "db": runtimeConfig.Redis.DB, "prefix": runtimeConfig.Redis.Prefix, "hash": runtimeConfig.Redis.Hash, "version_key": runtimeConfig.Redis.VersionKey, "tls": runtimeConfig.Redis.TLS},
	}
}

func redactedSourceAddress(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || isPlainHostPort(trimmed) {
		return trimmed
	}
	return redactedSourceURL(trimmed)
}

func isPlainHostPort(raw string) bool {
	host, portText, err := net.SplitHostPort(raw)
	if err != nil || strings.TrimSpace(host) == "" {
		return false
	}
	parsed, err := url.Parse("//" + raw)
	if err != nil || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "" || parsed.Hostname() == "" {
		return false
	}
	port, err := strconv.Atoi(portText)
	return err == nil && port >= 1 && port <= 65535
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
