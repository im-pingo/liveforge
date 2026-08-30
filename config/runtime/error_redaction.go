package runtime

import (
	"net/url"
	"regexp"
	"strings"
)

var errorURLPattern = regexp.MustCompile(`(?i)(?:https?|rediss?|consul)://[^\s"'<>]+`)

// RedactError removes URL credentials, query values, fragments, and line
// breaks before an error crosses a logging or management API boundary.
func RedactError(err error) string {
	if err == nil {
		return ""
	}
	message := strings.NewReplacer("\r", " ", "\n", " ").Replace(err.Error())
	return errorURLPattern.ReplaceAllStringFunc(message, redactErrorURL)
}

func redactErrorURL(raw string) string {
	trailing := ""
	for len(raw) > 0 && strings.ContainsRune(".,;:)", rune(raw[len(raw)-1])) {
		trailing = string(raw[len(raw)-1]) + trailing
		raw = raw[:len(raw)-1]
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "REDACTED_URL" + trailing
	}
	parsed.User = nil
	if parsed.Path != "" && parsed.Path != "/" {
		parsed.Path = "/REDACTED"
		parsed.RawPath = ""
	}
	parsed.RawQuery = "__liveforge_redacted__=1"
	parsed.ForceQuery = false
	parsed.Fragment = ""
	return parsed.String() + trailing
}
