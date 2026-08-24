package runtime

import (
	"fmt"
	"net/url"
	"strings"
)

func requireURL(raw, field string) (*url.URL, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("%s is required", field)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("%s must be an absolute URL", field)
	}
	return u, nil
}

func documentFromKeyValues(values map[string]string) ([]byte, error) {
	if len(values) == 0 {
		return nil, fmt.Errorf("configuration key snapshot is empty")
	}
	for _, key := range []string{"config", "config.yaml", "config.yml", "config.json"} {
		if value, ok := values[key]; ok {
			return []byte(value), nil
		}
	}
	root := make(map[string]any)
	for key, value := range values {
		parts := strings.FieldsFunc(strings.Trim(key, "./"), func(r rune) bool { return r == '.' || r == '/' })
		if len(parts) == 0 {
			continue
		}
		current := root
		for _, part := range parts[:len(parts)-1] {
			next, ok := current[part].(map[string]any)
			if !ok {
				next = make(map[string]any)
				current[part] = next
			}
			current = next
		}
		current[parts[len(parts)-1]] = parseScalar(value)
	}
	return marshalDeterministic(root)
}

func parseScalar(value string) any {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true":
		return true
	case "false":
		return false
	case "null":
		return nil
	}
	return value
}
