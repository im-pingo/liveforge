package config

import (
	"fmt"
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

func validAPIRole(role string) bool {
	switch role {
	case "viewer", "operator", "admin":
		return true
	default:
		return false
	}
}
