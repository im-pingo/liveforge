package config

import (
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestValidateRejectsUnsafeAndInvalidRuntimeValues(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Config)
		want string
	}{
		{"zero ring", func(c *Config) { c.Stream.RingBufferSize = 0 }, "ring_buffer_size"},
		{"bad auth mode", func(c *Config) { c.Auth.Publish.Mode = "magic" }, "publish.mode"},
		{"bad auth stage", func(c *Config) { c.Auth.Subscribe.Stage = "after_media" }, "subscribe.stage"},
		{"active token without secret", func(c *Config) {
			c.Auth.Enabled = true
			c.Auth.Publish.Mode = "token"
			c.Auth.Publish.Token.Secret = ""
		}, "publish.token.secret"},
		{"active callback without URL", func(c *Config) {
			c.Auth.Enabled = true
			c.Auth.Subscribe.Mode = "callback"
			c.Auth.Subscribe.Callback.URL = ""
		}, "subscribe.callback.url"},
		{"active combined mode without callback URL", func(c *Config) {
			c.Auth.Enabled = true
			c.Auth.Publish.Mode = "token+callback"
			c.Auth.Publish.Token.Secret = "secret"
			c.Auth.Publish.Callback.URL = ""
		}, "publish.callback.url"},
		{"unsupported token algorithm", func(c *Config) {
			c.Auth.Publish.Token.Algorithm = "RS256"
		}, "publish.token.algorithm"},
		{"bad rtsp range", func(c *Config) { c.RTSP.RTPPortRange = []int{20000, 10000} }, "rtsp.rtp_port_range"},
		{"short srt secret", func(c *Config) { c.SRT.Passphrase = "short" }, "srt.passphrase"},
		{"bad srt key", func(c *Config) { c.SRT.PBKeyLen = 15 }, "srt.pbkeylen"},
		{"unknown record placeholder", func(c *Config) { c.Record.Path = "/record/{camera}.flv" }, "record.path"},
		{"unknown dvr placeholder", func(c *Config) { c.DVR.Path = "/dvr/{camera}" }, "dvr.path"},
		{"remote default console", func(c *Config) {
			c.API.Enabled = true
			c.API.Listen = ":8090"
			c.API.Console.Username = "admin"
			c.API.Console.Password = "admin"
		}, "allow_insecure_default_credentials"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validTestConfig("test")
			tt.edit(cfg)
			err := Validate(cfg)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestValidateAllowsEmptyCredentialsWhenAuthOrRuleIsDisabled(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Config)
	}{
		{"auth disabled", func(c *Config) {
			c.Auth.Enabled = false
			c.Auth.Publish.Mode = "token+callback"
			c.Auth.Publish.Token.Secret = ""
			c.Auth.Publish.Callback.URL = ""
		}},
		{"rule disabled", func(c *Config) {
			c.Auth.Enabled = true
			c.Auth.Publish.Mode = "none"
			c.Auth.Publish.Token.Secret = ""
			c.Auth.Publish.Callback.URL = ""
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validTestConfig("inactive credentials")
			tt.edit(cfg)
			if err := Validate(cfg); err != nil {
				t.Fatalf("Validate rejected inactive empty credentials: %v", err)
			}
		})
	}
}

func TestNormalizeCanonicalizesSupportedTokenAlgorithm(t *testing.T) {
	cfg := validTestConfig("normalize algorithm")
	cfg.Auth.Publish.Token.Algorithm = " hs256 "
	cfg.Auth.Subscribe.Token.Algorithm = ""
	normalize(cfg)
	if cfg.Auth.Publish.Token.Algorithm != "HS256" || cfg.Auth.Subscribe.Token.Algorithm != "HS256" {
		t.Fatalf("normalized algorithms = %q/%q, want HS256/HS256",
			cfg.Auth.Publish.Token.Algorithm, cfg.Auth.Subscribe.Token.Algorithm)
	}
}

func TestValidateAllowsLocalDefaultAndExplicitRemoteOptIn(t *testing.T) {
	local := validTestConfig("local")
	local.API.Enabled = true
	local.API.Listen = "127.0.0.1:8090"
	local.API.Console.Username = "admin"
	local.API.Console.Password = "admin"
	if err := Validate(local); err != nil {
		t.Fatalf("local default rejected: %v", err)
	}

	remote := validTestConfig("remote")
	remote.API.Enabled = true
	remote.API.Listen = ":8090"
	remote.API.Console.Username = "admin"
	remote.API.Console.Password = "admin"
	remote.API.Console.AllowInsecureDefaultCredentials = true
	if err := Validate(remote); err != nil {
		t.Fatalf("explicit remote opt-in rejected: %v", err)
	}
}

func TestValidateRejectsHashedDefaultConsoleCredentialsOnRemoteListen(t *testing.T) {
	passwordHash, err := bcrypt.GenerateFromPassword([]byte("admin"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	cfg := validTestConfig("remote hashed default")
	cfg.API.Enabled = true
	cfg.API.Listen = ":8090"
	cfg.API.Console.Username = "admin"
	cfg.API.Console.Password = ""
	cfg.API.Console.PasswordHash = string(passwordHash)

	err = Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "allow_insecure_default_credentials") {
		t.Fatalf("Validate error = %v, want insecure-default rejection", err)
	}
}

func TestShippedConfigKeepsDefaultCredentialsOnLoopback(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "configs", "liveforge.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.API.Listen != "127.0.0.1:8090" {
		t.Fatalf("api.listen = %q, want loopback-only default", cfg.API.Listen)
	}
	if cfg.API.Console.Username != "admin" || cfg.API.Console.Password != "admin" {
		t.Fatalf("local default credentials = %q/%q, want admin/admin", cfg.API.Console.Username, cfg.API.Console.Password)
	}
	if cfg.API.Console.AllowInsecureDefaultCredentials {
		t.Fatal("shipped config unexpectedly opts into insecure remote default credentials")
	}
}
