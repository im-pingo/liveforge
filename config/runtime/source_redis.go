package runtime

import (
	"context"
	"crypto/tls"
	"fmt"
	"sort"
	"strings"

	"github.com/redis/go-redis/v9"
	"gopkg.in/yaml.v3"
)

// RedisSourceOptions configures hash or key-prefix reads from Redis.
type RedisSourceOptions struct {
	Addr       string
	Username   string
	Password   string
	DB         int
	Prefix     string
	Hash       string
	VersionKey string
	TLSConfig  *tls.Config
	Client     *redis.Client
}

type RedisSource struct {
	client     *redis.Client
	prefix     string
	hash       string
	versionKey string
}

func NewRedisSource(opts RedisSourceOptions) (*RedisSource, error) {
	if opts.Client == nil {
		if opts.Addr == "" {
			return nil, fmt.Errorf("redis addr is required")
		}
		opts.Client = redis.NewClient(&redis.Options{Addr: opts.Addr, Username: opts.Username, Password: opts.Password, DB: opts.DB, TLSConfig: opts.TLSConfig})
	}
	if opts.Hash == "" && opts.Prefix == "" {
		return nil, fmt.Errorf("redis hash or prefix is required")
	}
	return &RedisSource{client: opts.Client, prefix: opts.Prefix, hash: opts.Hash, versionKey: opts.VersionKey}, nil
}

func (s *RedisSource) Name() string { return "redis" }

func (s *RedisSource) Load(ctx context.Context, previous Version) (Snapshot, error) {
	values := make(map[string]string)
	if s.hash != "" {
		fields, err := s.client.HGetAll(ctx, s.hash).Result()
		if err != nil {
			return Snapshot{}, fmt.Errorf("read redis hash: %w", err)
		}
		for key, value := range fields {
			values[key] = value
		}
	} else {
		var keys []string
		var cursor uint64
		for {
			batch, next, err := s.client.Scan(ctx, cursor, s.prefix+"*", 256).Result()
			if err != nil {
				return Snapshot{}, fmt.Errorf("scan redis config keys: %w", err)
			}
			keys = append(keys, batch...)
			cursor = next
			if cursor == 0 {
				break
			}
		}
		sort.Strings(keys)
		pipe := s.client.Pipeline()
		commands := make([]*redis.StringCmd, 0, len(keys))
		for _, key := range keys {
			commands = append(commands, pipe.Get(ctx, key))
		}
		if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
			return Snapshot{}, fmt.Errorf("read redis config keys: %w", err)
		}
		for i, key := range keys {
			value, err := commands[i].Result()
			if err == nil {
				values[strings.TrimPrefix(key, s.prefix)] = value
			}
		}
	}
	data, err := documentFromKeyValues(values)
	if err != nil {
		return Snapshot{}, err
	}
	version := ""
	if s.versionKey != "" {
		version, err = s.client.Get(ctx, s.versionKey).Result()
		if err != nil && err != redis.Nil {
			return Snapshot{}, fmt.Errorf("read redis config version: %w", err)
		}
	}
	return Snapshot{Data: data, Version: version}, nil
}

func (s *RedisSource) Close() error { return s.client.Close() }

func marshalDeterministic(value any) ([]byte, error) {
	// yaml.v3 sorts string map keys, producing stable bytes for hashing.
	b, err := yaml.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("marshal key snapshot: %w", err)
	}
	return b, nil
}
