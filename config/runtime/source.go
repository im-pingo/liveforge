package runtime

import (
	"fmt"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

var (
	canonicalDecimalInteger = regexp.MustCompile(`^(?:0|-[1-9][0-9]*|[1-9][0-9]*)$`)
	canonicalDecimalFloat   = regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+(?:[eE][+-]?[0-9]+)?|[eE][+-]?[0-9]+)$`)
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
	trimmed := strings.TrimSpace(value)
	switch strings.ToLower(trimmed) {
	case "true":
		return true
	case "false":
		return false
	case "null":
		return nil
	}
	if canonicalDecimalInteger.MatchString(trimmed) {
		if parsed, err := strconv.ParseInt(trimmed, 10, 64); err == nil {
			return parsed
		}
	}
	if canonicalDecimalFloat.MatchString(trimmed) {
		if parsed, err := strconv.ParseFloat(trimmed, 64); err == nil && !math.IsInf(parsed, 0) && !math.IsNaN(parsed) {
			return parsed
		}
	}
	return value
}
