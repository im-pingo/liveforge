package config

import (
	"fmt"
	"net"
	"regexp"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

func Validate(cfg *Config) error {
	if cfg == nil {
		return fmt.Errorf("config is nil")
	}
	if cfg.Stream.RingBufferSize <= 0 {
		return fmt.Errorf("stream.ring_buffer_size must be greater than zero")
	}
	if cfg.Limits.RateLimit.Enabled && (cfg.Limits.RateLimit.Rate <= 0 || cfg.Limits.RateLimit.Burst <= 0) {
		return fmt.Errorf("limits.rate_limit.rate and burst must be greater than zero when enabled")
	}
	if err := validateAuthRule("publish", cfg.Auth.Publish, cfg.Auth.Enabled); err != nil {
		return err
	}
	if err := validateAuthRule("subscribe", cfg.Auth.Subscribe, cfg.Auth.Enabled); err != nil {
		return err
	}
	for name, ports := range map[string][]int{
		"rtsp.rtp_port_range":    cfg.RTSP.RTPPortRange,
		"webrtc.udp_port_range":  cfg.WebRTC.UDPPortRange,
		"gb28181.rtp_port_range": cfg.GB28181.RTPPortRange,
	} {
		if err := validatePortRange(name, ports); err != nil {
			return err
		}
	}
	for _, value := range cfg.Limits.RateLimit.TrustedProxies {
		value = strings.TrimSpace(value)
		if value == "" {
			return fmt.Errorf("limits.rate_limit.trusted_proxies must not contain empty entries")
		}
		if net.ParseIP(value) == nil {
			if _, _, err := net.ParseCIDR(value); err != nil {
				return fmt.Errorf("limits.rate_limit.trusted_proxies entry %q is not an IP or CIDR: %w", value, err)
			}
		}
	}
	if cfg.SRT.Passphrase != "" && (len(cfg.SRT.Passphrase) < 10 || len(cfg.SRT.Passphrase) > 79) {
		return fmt.Errorf("srt.passphrase must contain 10 to 79 characters")
	}
	if cfg.SIP.Auth.Enabled && strings.TrimSpace(cfg.SIP.Auth.Password) == "" {
		return fmt.Errorf("sip.auth.password must not be empty when digest authentication is enabled")
	}
	switch cfg.SRT.PBKeyLen {
	case 0, 16, 24, 32:
	default:
		return fmt.Errorf("srt.pbkeylen must be one of 0, 16, 24, or 32")
	}
	if cfg.API.Enabled && usesDefaultConsoleCredentials(cfg.API.Console) &&
		!cfg.API.Console.AllowInsecureDefaultCredentials && !isLoopbackListen(cfg.API.Listen) {
		return fmt.Errorf("api.console.allow_insecure_default_credentials must be true to use admin/admin on non-loopback listen %q", cfg.API.Listen)
	}
	if err := validatePathPlaceholders("record.path", cfg.Record.Path, map[string]bool{
		"stream": true, "stream_key": true, "date": true, "time": true, "ext": true,
	}); err != nil {
		return err
	}
	if err := validatePathPlaceholders("dvr.path", cfg.DVR.Path, map[string]bool{
		"stream": true, "stream_key": true,
	}); err != nil {
		return err
	}
	return nil
}

func usesDefaultConsoleCredentials(cfg ConsoleConfig) bool {
	if cfg.Username != "admin" {
		return false
	}
	if cfg.PasswordHash != "" {
		return bcrypt.CompareHashAndPassword([]byte(cfg.PasswordHash), []byte("admin")) == nil
	}
	return cfg.Password == "admin"
}

var pathPlaceholderPattern = regexp.MustCompile(`\{([^{}]+)\}`)

func validatePathPlaceholders(name, template string, allowed map[string]bool) error {
	for _, match := range pathPlaceholderPattern.FindAllStringSubmatch(template, -1) {
		if !allowed[match[1]] {
			return fmt.Errorf("%s contains unknown placeholder %q", name, match[0])
		}
	}
	return nil
}

func validateAuthRule(name string, rule AuthRuleConfig, authEnabled bool) error {
	switch rule.Mode {
	case "", "none", "token", "callback", "token+callback":
	default:
		return fmt.Errorf("auth.%s.mode %q is invalid", name, rule.Mode)
	}
	switch rule.Stage {
	case "", "pre_session", "post_connect":
	default:
		return fmt.Errorf("auth.%s.stage %q is invalid", name, rule.Stage)
	}
	if rule.Token.Algorithm != "" && !strings.EqualFold(rule.Token.Algorithm, "HS256") {
		return fmt.Errorf("auth.%s.token.algorithm must be HS256", name)
	}
	if !authEnabled {
		return nil
	}
	switch rule.Mode {
	case "token":
		if rule.Token.Secret == "" {
			return fmt.Errorf("auth.%s.token.secret must not be empty for token mode", name)
		}
	case "callback":
		if strings.TrimSpace(rule.Callback.URL) == "" {
			return fmt.Errorf("auth.%s.callback.url must not be empty for callback mode", name)
		}
	case "token+callback":
		if rule.Token.Secret == "" {
			return fmt.Errorf("auth.%s.token.secret must not be empty for token+callback mode", name)
		}
		if strings.TrimSpace(rule.Callback.URL) == "" {
			return fmt.Errorf("auth.%s.callback.url must not be empty for token+callback mode", name)
		}
	}
	return nil
}

func validatePortRange(name string, ports []int) error {
	if len(ports) == 0 {
		return nil
	}
	if len(ports) != 2 || ports[0] < 1 || ports[1] > 65535 || ports[0] > ports[1] {
		return fmt.Errorf("%s must contain an ascending [min, max] pair", name)
	}
	return nil
}

func isLoopbackListen(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return false
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
