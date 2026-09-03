package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Validate checks configuration invariants shared by bootstrap and runtime
// loading paths.
func Validate(cfg *Config) error {
	if cfg == nil {
		return fmt.Errorf("configuration is nil")
	}

	username := strings.TrimSpace(cfg.API.Console.Username)
	password := strings.TrimSpace(cfg.API.Console.Password)
	if cfg.API.Console.Username != "" || cfg.API.Console.Password != "" {
		if username == "" || password == "" {
			return fmt.Errorf("api.console.username and api.console.password must be configured together and must not be blank")
		}
	}
	if role := strings.ToLower(strings.TrimSpace(cfg.API.Console.Role)); role != "" && !validAPIRole(role) {
		return fmt.Errorf("api.console.role must be viewer, operator, or admin")
	}
	if cfg.HTTP.LLHLS.Enabled && cfg.HTTP.LLHLS.SegmentDuration <= 0 {
		return fmt.Errorf("http_stream.llhls.segment_duration must be greater than zero")
	}
	if cfg.Stream.RingBufferSize <= 0 {
		return fmt.Errorf("stream.ring_buffer_size must be greater than zero")
	}
	if cfg.Stream.GOPCacheNum < 0 {
		return fmt.Errorf("stream.gop_cache_num must not be negative")
	}
	if cfg.Stream.GOPCacheMaxFrames < 0 {
		return fmt.Errorf("stream.gop_cache_max_frames must not be negative")
	}
	if cfg.Stream.GOPCacheMaxDuration < 0 {
		return fmt.Errorf("stream.gop_cache_max_duration must not be negative")
	}
	if cfg.Stream.GOPCacheMaxBytes < 0 {
		return fmt.Errorf("stream.gop_cache_max_bytes must not be negative")
	}
	if cfg.Stream.GOPCache && cfg.Stream.GOPCacheNum > 0 &&
		cfg.Stream.GOPCacheMaxFrames == 0 && cfg.Stream.GOPCacheMaxBytes == 0 {
		return fmt.Errorf("stream.gop_cache_max_frames or stream.gop_cache_max_bytes must be positive when GOP cache is enabled")
	}
	if err := ValidateRecordConfig(cfg.Record); err != nil {
		return err
	}
	if cfg.Metrics.StreamDetailLimit < 0 {
		return fmt.Errorf("metrics.stream_detail_limit must not be negative")
	}
	for i, value := range cfg.Limits.RateLimit.TrustedProxies {
		value = strings.TrimSpace(value)
		if value == "" {
			return fmt.Errorf("limits.rate_limit.trusted_proxies[%d] must not be empty", i)
		}
		if strings.Contains(value, "/") {
			if _, _, err := net.ParseCIDR(value); err == nil {
				continue
			}
		} else if net.ParseIP(value) != nil {
			continue
		}
		return fmt.Errorf("limits.rate_limit.trusted_proxies[%d] must be an IP address or CIDR network", i)
	}

	seenTokens := make(map[string]struct{}, len(cfg.API.Auth.Tokens))
	for i, binding := range cfg.API.Auth.Tokens {
		if binding.Token == "" {
			return fmt.Errorf("api.auth.tokens[%d].token must not be empty", i)
		}
		if !validAPIRole(strings.ToLower(strings.TrimSpace(binding.Role))) {
			return fmt.Errorf("api.auth.tokens[%d].role must be viewer, operator, or admin", i)
		}
		if binding.Token == cfg.API.Auth.BearerToken && cfg.API.Auth.BearerToken != "" {
			return fmt.Errorf("api.auth.tokens[%d].token must not duplicate api.auth.bearer_token", i)
		}
		if _, exists := seenTokens[binding.Token]; exists {
			return fmt.Errorf("api.auth.tokens contains a duplicate token")
		}
		seenTokens[binding.Token] = struct{}{}
	}
	return nil
}

// ValidateRecordConfig checks recording-specific values without requiring a
// complete root configuration. Empty and zero max_size values disable size
// rotation; otherwise the value is a non-negative decimal byte count with an
// optional B, KB, MB, or GB suffix.
func ValidateRecordConfig(cfg RecordConfig) error {
	switch format := strings.ToLower(strings.TrimSpace(cfg.Format)); format {
	case "", "flv", "fmp4", "mp4", "ts", "hls":
	default:
		return fmt.Errorf("record.format must be flv, fmp4, mp4, ts, or hls")
	}
	if cfg.Path != "" {
		if _, err := ResolveUserPath(cfg.Path); err != nil {
			return fmt.Errorf("record.path: %w", err)
		}
	}
	if cfg.Segment.MaxSize == "" {
		return nil
	}
	if _, err := ParseByteSize(cfg.Segment.MaxSize); err != nil {
		return fmt.Errorf("record.segment.max_size: %w", err)
	}
	return nil
}

// ResolveUserPath expands environment variables and a leading ~/ to the
// current process user's home directory. Named-user paths such as ~alice are
// rejected because their meaning is host-dependent and could silently write
// recordings outside the operator's intended storage root.
func ResolveUserPath(value string) (string, error) {
	value = os.ExpandEnv(value)
	if value == "" {
		return value, nil
	}
	if value != "~" && !strings.HasPrefix(value, "~/") && !strings.HasPrefix(value, `~\`) {
		if strings.HasPrefix(value, "~") {
			return "", fmt.Errorf("named-user paths are not supported; use ~/... or an absolute path")
		}
		return value, nil
	}
	home, err := os.UserHomeDir()
	if err != nil || filepath.Clean(home) == "." {
		if err == nil {
			err = fmt.Errorf("home directory is unavailable")
		}
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	if value == "~" {
		return filepath.Clean(home), nil
	}
	rel := strings.TrimLeft(value[2:], `/\`)
	return filepath.Join(home, rel), nil
}

// ParseByteSize parses a decimal byte count with an optional binary suffix.
func ParseByteSize(value string) (int64, error) {
	value = strings.TrimSpace(strings.ToUpper(value))
	if value == "" {
		return 0, nil
	}

	multiplier := int64(1)
	switch {
	case strings.HasSuffix(value, "GB"):
		value = strings.TrimSuffix(value, "GB")
		multiplier = 1024 * 1024 * 1024
	case strings.HasSuffix(value, "MB"):
		value = strings.TrimSuffix(value, "MB")
		multiplier = 1024 * 1024
	case strings.HasSuffix(value, "KB"):
		value = strings.TrimSuffix(value, "KB")
		multiplier = 1024
	case strings.HasSuffix(value, "B"):
		value = strings.TrimSuffix(value, "B")
	}
	if value == "" {
		return 0, fmt.Errorf("must contain decimal digits")
	}
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return 0, fmt.Errorf("must be a non-negative integer with optional B, KB, MB, or GB suffix")
		}
	}
	n, err := strconv.ParseInt(value, 10, 63)
	if err != nil || n > (1<<63-1)/multiplier {
		return 0, fmt.Errorf("is too large")
	}
	return n * multiplier, nil
}

func validAPIRole(role string) bool {
	switch role {
	case "viewer", "operator", "admin":
		return true
	default:
		return false
	}
}
