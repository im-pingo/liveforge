package core

import (
	"fmt"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ValidateStreamKey accepts a canonical, relative slash-separated media key.
// Credentials and URL syntax must be parsed before this boundary.
func ValidateStreamKey(key string) error {
	if key == "" {
		return fmt.Errorf("stream key is empty")
	}
	if len(key) > 512 {
		return fmt.Errorf("stream key exceeds 512 bytes")
	}
	if !utf8.ValidString(key) {
		return fmt.Errorf("stream key is not valid UTF-8")
	}
	if strings.HasPrefix(key, "/") || path.IsAbs(key) {
		return fmt.Errorf("stream key must be relative")
	}
	if strings.ContainsAny(key, `\?#`) {
		return fmt.Errorf("stream key contains URL or path separator characters")
	}
	if path.Clean(key) != key {
		return fmt.Errorf("stream key is not canonical")
	}
	for _, segment := range strings.Split(key, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return fmt.Errorf("stream key contains an invalid path segment")
		}
	}
	for _, r := range key {
		if unicode.IsControl(r) {
			return fmt.Errorf("stream key contains control characters")
		}
	}
	return nil
}
