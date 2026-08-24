package runtime

import (
	"crypto/tls"
	"fmt"
	"strings"

	"github.com/im-pingo/liveforge/config"
)

// BuildSource selects a source from the bootstrap runtime configuration. The
// bootstrap path is used when the selected source is file and no explicit path
// is configured.
func BuildSource(cfg config.RuntimeConfig, bootstrapPath string) (NamedSource, error) {
	kind := strings.ToLower(strings.TrimSpace(cfg.Source))
	if kind == "" {
		kind = "file"
	}
	switch kind {
	case "file":
		path := cfg.File.Path
		if path == "" {
			path = bootstrapPath
		}
		return NewFileSource(path)
	case "http", "https":
		return NewHTTPSource(HTTPSourceOptions{URL: cfg.HTTP.URL, Token: cfg.HTTP.Token, MaxBytes: cfg.HTTP.MaxBytes})
	case "consul":
		return NewConsulSource(ConsulSourceOptions{Address: cfg.Consul.Address, Prefix: cfg.Consul.Prefix, Token: cfg.Consul.Token, MaxBytes: cfg.Consul.MaxBytes})
	case "redis":
		var tlsConfig *tls.Config
		if cfg.Redis.TLS {
			tlsConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		}
		return NewRedisSource(RedisSourceOptions{Addr: cfg.Redis.Addr, Username: cfg.Redis.Username, Password: cfg.Redis.Password, DB: cfg.Redis.DB, Prefix: cfg.Redis.Prefix, Hash: cfg.Redis.Hash, VersionKey: cfg.Redis.VersionKey, TLSConfig: tlsConfig})
	default:
		return nil, fmt.Errorf("unsupported runtime config source %q", kind)
	}
}
