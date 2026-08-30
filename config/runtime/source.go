package runtime

import (
	"fmt"
	"math"
	"net/url"
	"regexp"
	"sort"
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
	type flattenedValue struct {
		key       string
		value     string
		parts     []string
		canonical string
	}
	flattened := make([]flattenedValue, 0, len(values))
	for key, value := range values {
		parts := strings.FieldsFunc(strings.Trim(key, "./"), func(r rune) bool { return r == '.' || r == '/' })
		if len(parts) == 0 {
			continue
		}
		flattened = append(flattened, flattenedValue{key: key, value: value, parts: parts, canonical: strings.Join(parts, ".")})
	}
	sort.Slice(flattened, func(i, j int) bool {
		if flattened[i].canonical != flattened[j].canonical {
			return flattened[i].canonical < flattened[j].canonical
		}
		return flattened[i].key < flattened[j].key
	})
	for index := 1; index < len(flattened); index++ {
		previous, current := flattened[index-1], flattened[index]
		if previous.canonical == current.canonical {
			return nil, fmt.Errorf("flattened configuration keys %q and %q collide at %q", previous.key, current.key, current.canonical)
		}
		if strings.HasPrefix(current.canonical, previous.canonical+".") {
			return nil, fmt.Errorf("flattened configuration keys %q and %q collide at %q", previous.key, current.key, previous.canonical)
		}
	}
	for _, item := range flattened {
		parts, value := item.parts, item.value
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
