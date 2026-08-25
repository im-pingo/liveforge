package runtime

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

func TestConfigSchemaAcceptsRuntimeDefaultSentinels(t *testing.T) {
	document := map[string]any{
		"rtsp":    map[string]any{"rtp_port_range": []any{}},
		"webrtc":  map[string]any{"udp_port_range": []any{}},
		"gb28181": map[string]any{"rtp_port_range": []any{}},
		"sip":     map[string]any{"gateway": map[string]any{"max_calls": 0}},
		"cluster": map[string]any{"health_check": map[string]any{"evict_threshold": 0}},
		"api":     map[string]any{"audit": map[string]any{"max_entries": 0}},
		"runtime": map[string]any{"http": map[string]any{"max_bytes": 0}, "consul": map[string]any{"max_bytes": 0}},
	}

	schema := loadConfigSchema(t)
	if err := validateSchemaValue(schema, schema, document, "$"); err != nil {
		t.Fatalf("schema rejected source-supported default sentinels: %v", err)
	}

	const yamlDocument = `
rtsp:
  rtp_port_range: []
webrtc:
  udp_port_range: []
gb28181:
  rtp_port_range: []
sip:
  gateway:
    max_calls: 0
cluster:
  health_check:
    evict_threshold: 0
api:
  audit:
    max_entries: 0
runtime:
  http:
    max_bytes: 0
  consul:
    max_bytes: 0
`
	if _, err := ParseDocument([]byte(yamlDocument)); err != nil {
		t.Fatalf("runtime parser rejected documented default sentinels: %v", err)
	}
}

func TestConfigSchemaAcceptsNegativeScalarDefaultSentinels(t *testing.T) {
	schema := loadConfigSchema(t)
	document := map[string]any{
		"sip":     map[string]any{"gateway": map[string]any{"max_calls": -1}},
		"cluster": map[string]any{"health_check": map[string]any{"evict_threshold": -1}},
		"api":     map[string]any{"audit": map[string]any{"max_entries": -1}},
		"runtime": map[string]any{"http": map[string]any{"max_bytes": -1}, "consul": map[string]any{"max_bytes": -1}},
	}
	if err := validateSchemaValue(schema, schema, document, "$"); err != nil {
		t.Fatalf("schema rejected source-supported negative default sentinels: %v", err)
	}

	const yamlDocument = `
sip:
  gateway:
    max_calls: -1
cluster:
  health_check:
    evict_threshold: -1
api:
  audit:
    max_entries: -1
runtime:
  http:
    max_bytes: -1
  consul:
    max_bytes: -1
`
	cfg, err := ParseDocument([]byte(yamlDocument))
	if err != nil {
		t.Fatalf("runtime parser rejected source-supported negative default sentinels: %v", err)
	}
	if cfg.SIP.Gateway.MaxCalls != -1 || cfg.Cluster.HealthCheck.EvictThreshold != -1 ||
		cfg.API.Audit.MaxEntries != -1 || cfg.Runtime.HTTP.MaxBytes != -1 || cfg.Runtime.Consul.MaxBytes != -1 {
		t.Fatalf("runtime parser did not preserve negative sentinels: max_calls=%d evict_threshold=%d max_entries=%d http_max_bytes=%d consul_max_bytes=%d",
			cfg.SIP.Gateway.MaxCalls, cfg.Cluster.HealthCheck.EvictThreshold, cfg.API.Audit.MaxEntries,
			cfg.Runtime.HTTP.MaxBytes, cfg.Runtime.Consul.MaxBytes)
	}
}

func TestConfigSchemaRejectsRuntimeInvalidPortRanges(t *testing.T) {
	schema := loadConfigSchema(t)
	tests := []struct {
		name         string
		document     map[string]any
		yamlDocument string
	}{
		{
			name:         "rtsp range",
			document:     map[string]any{"rtsp": map[string]any{"rtp_port_range": []any{-1, 2}}},
			yamlDocument: "rtsp:\n  rtp_port_range: [-1, 2]\n",
		},
		{
			name:         "webrtc range",
			document:     map[string]any{"webrtc": map[string]any{"udp_port_range": []any{-1, 2}}},
			yamlDocument: "webrtc:\n  udp_port_range: [-1, 2]\n",
		},
		{
			name:         "gb28181 range",
			document:     map[string]any{"gb28181": map[string]any{"rtp_port_range": []any{-1, 2}}},
			yamlDocument: "gb28181:\n  rtp_port_range: [-1, 2]\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := validateSchemaValue(schema, schema, tt.document, "$"); err == nil {
				t.Fatal("schema accepted a runtime-invalid port range")
			}
			if _, err := ParseDocument([]byte(tt.yamlDocument)); err == nil {
				t.Fatal("runtime parser accepted a runtime-invalid port range")
			}
		})
	}
}

func TestConfigSchemaAcceptsCheckedInSample(t *testing.T) {
	schema := loadConfigSchema(t)
	data, err := os.ReadFile("../../configs/liveforge.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var yamlDocument any
	if err := yaml.Unmarshal(data, &yamlDocument); err != nil {
		t.Fatalf("parse sample YAML: %v", err)
	}
	jsonDocument, err := json.Marshal(yamlDocument)
	if err != nil {
		t.Fatalf("convert sample YAML to JSON: %v", err)
	}
	var document any
	if err := json.Unmarshal(jsonDocument, &document); err != nil {
		t.Fatalf("decode sample JSON form: %v", err)
	}
	if err := validateSchemaValue(schema, schema, document, "$"); err != nil {
		t.Fatalf("schema rejected configs/liveforge.yaml: %v", err)
	}
}

func loadConfigSchema(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile("../../docs/config/config.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("parse config schema: %v", err)
	}
	return schema
}

// validateSchemaValue executes the JSON Schema keywords used by the focused
// sentinel fixtures, including local refs and combinators.
func validateSchemaValue(root, schema map[string]any, value any, path string) error {
	if ref, ok := schema["$ref"].(string); ok {
		resolved, err := resolveLocalSchemaRef(root, ref)
		if err != nil {
			return err
		}
		return validateSchemaValue(root, resolved, value, path)
	}
	if branches, ok := schema["anyOf"].([]any); ok {
		if !anySchemaBranchAccepts(root, branches, value, path) {
			return fmt.Errorf("%s does not satisfy anyOf", path)
		}
	}
	if branches, ok := schema["oneOf"].([]any); ok {
		accepted := 0
		for _, branch := range branches {
			if validateSchemaValue(root, branch.(map[string]any), value, path) == nil {
				accepted++
			}
		}
		if accepted != 1 {
			return fmt.Errorf("%s satisfies %d oneOf branches", path, accepted)
		}
	}

	if declaredType, ok := schema["type"]; ok && !matchesSchemaType(declaredType, value) {
		return fmt.Errorf("%s does not match type %v", path, declaredType)
	}

	if object, ok := value.(map[string]any); ok {
		for _, required := range stringValues(schema["required"]) {
			if _, exists := object[required]; !exists {
				return fmt.Errorf("%s.%s is required", path, required)
			}
		}
		properties, _ := schema["properties"].(map[string]any)
		for key, child := range object {
			childSchema, exists := properties[key]
			if !exists {
				if schema["additionalProperties"] == false {
					return fmt.Errorf("%s.%s is not allowed", path, key)
				}
				continue
			}
			if err := validateSchemaValue(root, childSchema.(map[string]any), child, path+"."+key); err != nil {
				return err
			}
		}
	}
	if array, ok := value.([]any); ok {
		if minimum, ok := schemaNumber(schema["minItems"]); ok && len(array) < int(minimum) {
			return fmt.Errorf("%s has %d items, minimum is %d", path, len(array), int(minimum))
		}
		if maximum, ok := schemaNumber(schema["maxItems"]); ok && len(array) > int(maximum) {
			return fmt.Errorf("%s has %d items, maximum is %d", path, len(array), int(maximum))
		}
		if itemSchema, ok := schema["items"].(map[string]any); ok {
			for i, item := range array {
				if err := validateSchemaValue(root, itemSchema, item, path+"["+strconv.Itoa(i)+"]"); err != nil {
					return err
				}
			}
		}
		if prefixItems, ok := schema["prefixItems"].([]any); ok {
			for i, itemSchema := range prefixItems {
				if i >= len(array) {
					break
				}
				if err := validateSchemaValue(root, itemSchema.(map[string]any), array[i], path+"["+strconv.Itoa(i)+"]"); err != nil {
					return err
				}
			}
			if schema["items"] == false && len(array) > len(prefixItems) {
				return fmt.Errorf("%s has items beyond prefixItems", path)
			}
		}
	}
	if text, ok := value.(string); ok {
		if minimum, ok := schemaNumber(schema["minLength"]); ok && utf8.RuneCountInString(text) < int(minimum) {
			return fmt.Errorf("%s is shorter than %d characters", path, int(minimum))
		}
		if pattern, ok := schema["pattern"].(string); ok {
			matched, err := regexp.MatchString(pattern, text)
			if err != nil {
				return fmt.Errorf("invalid schema pattern %q: %w", pattern, err)
			}
			if !matched {
				return fmt.Errorf("%s does not match %q", path, pattern)
			}
		}
	}
	if options, ok := schema["enum"].([]any); ok {
		matched := false
		for _, option := range options {
			if schemaValuesEqual(option, value) {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("%s is not in enum %v", path, options)
		}
	}

	if number, ok := schemaNumber(value); ok {
		if minimum, ok := schemaNumber(schema["minimum"]); ok && number < minimum {
			return fmt.Errorf("%s is %v, minimum is %v", path, number, minimum)
		}
		if maximum, ok := schemaNumber(schema["maximum"]); ok && number > maximum {
			return fmt.Errorf("%s is %v, maximum is %v", path, number, maximum)
		}
		if minimum, ok := schemaNumber(schema["exclusiveMinimum"]); ok && number <= minimum {
			return fmt.Errorf("%s is %v, exclusive minimum is %v", path, number, minimum)
		}
	}
	return nil
}

func matchesSchemaType(declared any, value any) bool {
	switch declared := declared.(type) {
	case string:
		return matchesSingleSchemaType(declared, value)
	case []any:
		for _, candidate := range declared {
			if matchesSingleSchemaType(candidate.(string), value) {
				return true
			}
		}
	}
	return false
}

func matchesSingleSchemaType(declared string, value any) bool {
	switch declared {
	case "object":
		object, ok := value.(map[string]any)
		return ok && object != nil
	case "array":
		_, ok := value.([]any)
		return ok
	case "integer":
		number, ok := schemaNumber(value)
		return ok && math.Trunc(number) == number
	case "number":
		_, ok := schemaNumber(value)
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "null":
		return value == nil
	}
	return false
}

func anySchemaBranchAccepts(root map[string]any, branches []any, value any, path string) bool {
	for _, branch := range branches {
		if validateSchemaValue(root, branch.(map[string]any), value, path) == nil {
			return true
		}
	}
	return false
}

func resolveLocalSchemaRef(root map[string]any, ref string) (map[string]any, error) {
	if !strings.HasPrefix(ref, "#/") {
		return nil, fmt.Errorf("unsupported schema reference %q", ref)
	}
	var current any = root
	for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		object, ok := current.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("schema reference %q crosses a non-object", ref)
		}
		current, ok = object[part]
		if !ok {
			return nil, fmt.Errorf("schema reference %q does not resolve", ref)
		}
	}
	resolved, ok := current.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("schema reference %q does not resolve to an object", ref)
	}
	return resolved, nil
}

func schemaNumber(value any) (float64, bool) {
	switch number := value.(type) {
	case int:
		return float64(number), true
	case int64:
		return float64(number), true
	case float64:
		return number, true
	default:
		return 0, false
	}
}

func stringValues(value any) []string {
	values, _ := value.([]any)
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, value.(string))
	}
	return out
}

func schemaValuesEqual(left, right any) bool {
	if leftNumber, ok := schemaNumber(left); ok {
		rightNumber, rightOK := schemaNumber(right)
		return rightOK && leftNumber == rightNumber
	}
	return reflect.DeepEqual(left, right)
}
