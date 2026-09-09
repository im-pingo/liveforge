package api

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const auditMaxLineBytes = 64 << 10

var auditURLPattern = regexp.MustCompile(`(?i)[a-z][a-z0-9+.-]*://[^\s"'<>]+`)
var auditCredentialPattern = regexp.MustCompile(`(?i)(?:token|secret|password|authorization|credential|api[_-]?key)\s*[:=]`)

func sanitizeAuditEntry(entry AuditEntry) AuditEntry {
	entry.RequestID = sanitizeAuditText(entry.RequestID, 512)
	entry.Principal = sanitizeAuditText(entry.Principal, 512)
	entry.Role = sanitizeAuditText(entry.Role, 512)
	entry.Action = sanitizeAuditText(entry.Action, 512)
	entry.Resource = sanitizeAuditText(entry.Resource, 2048)
	entry.Result = sanitizeAuditText(entry.Result, 512)
	entry.RemoteAddr = sanitizeAuditText(entry.RemoteAddr, 512)
	if entry.Time.Year() < 0 || entry.Time.Year() > 9999 {
		entry.Time = time.Now().UTC()
	}
	metadata := make(map[string]string)
	visited := 0
	for key, value := range entry.Metadata {
		visited++
		if visited > 64 || len(metadata) == 16 {
			break
		}
		if len(key) > 64 || sensitiveAuditKey(key) {
			continue
		}
		metadata[sanitizeAuditText(key, 64)] = sanitizeAuditText(value, 256)
	}
	entry.Metadata = metadata
	return entry
}

func sanitizeAuditText(value string, limit int) string {
	if len(value) > limit {
		value = value[:limit]
	}
	value = strings.ToValidUTF8(value, "")
	trimmed := strings.TrimSpace(value)
	if strings.ContainsAny(value, "\r\n") || strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") && json.Valid([]byte(trimmed)) || strings.Contains(strings.ToLower(value), "bearer ") || auditCredentialPattern.MatchString(value) {
		return "[REDACTED]"
	}
	value = auditURLPattern.ReplaceAllStringFunc(value, redactAuditURL)
	if strings.HasPrefix(value, "//") {
		value = redactAuditURL(value)
	}
	if strings.HasPrefix(value, "/") {
		if parsed, err := url.Parse(value); err == nil && (parsed.RawQuery != "" || parsed.Fragment != "") {
			value = parsed.EscapedPath()
		}
	}
	return strings.Clone(value)
}

func redactAuditURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" || parsed.Opaque != "" {
		return "[REDACTED]"
	}
	parsed.User = nil
	if parsed.Path != "" && parsed.Path != "/" {
		parsed.Path = "/REDACTED"
		parsed.RawPath = ""
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		parsed.RawQuery = "redacted=1"
	}
	parsed.ForceQuery = false
	parsed.Fragment = ""
	return parsed.String()
}
