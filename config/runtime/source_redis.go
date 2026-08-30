package runtime

import (
	"context"
	"crypto/tls"
	"errors"
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
	MaxBytes   int64
}

type RedisSource struct {
	client     *redis.Client
	prefix     string
	hash       string
	versionKey string
	maxBytes   int64
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
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = DefaultSourceMaxBytes
	}
	return &RedisSource{client: opts.Client, prefix: opts.Prefix, hash: opts.Hash, versionKey: opts.VersionKey, maxBytes: opts.MaxBytes}, nil
}

func (s *RedisSource) Name() string { return "redis" }

func (s *RedisSource) Load(ctx context.Context, previous Version) (Snapshot, error) {
	var (
		values map[string]string
		err    error
	)
	if s.hash != "" {
		values, err = s.loadHash(ctx)
	} else {
		values, err = s.loadPrefix(ctx)
	}
	if err != nil {
		return Snapshot{}, err
	}
	data, err := documentFromKeyValuesWithLimit(values, s.maxBytes)
	if err != nil {
		return Snapshot{}, err
	}
	version := ""
	if s.versionKey != "" {
		version, err = s.readString(ctx, s.versionKey)
		if err != nil {
			return Snapshot{}, fmt.Errorf("read redis config version: %w", err)
		}
	}
	return Snapshot{Data: data, Version: version}, nil
}

var completeConfigKeys = []string{"config", "config.yaml", "config.yml", "config.json"}

func (s *RedisSource) loadHash(ctx context.Context) (map[string]string, error) {
	for _, field := range completeConfigKeys {
		exists, err := s.client.HExists(ctx, s.hash, field).Result()
		if err != nil {
			return nil, fmt.Errorf("check redis hash field: %w", err)
		}
		if !exists {
			continue
		}
		value, err := s.readHashString(ctx, field)
		if err != nil {
			return nil, err
		}
		return map[string]string{field: value}, nil
	}

	fields, err := s.hashFieldNames(ctx)
	if err != nil {
		return nil, err
	}
	values := make(map[string]string, len(fields))
	var materializedBytes int64
	for start := 0; start < len(fields); start += 128 {
		end := start + 128
		if end > len(fields) {
			end = len(fields)
		}
		lengthPipe := s.client.Pipeline()
		lengthCommands := make([]*redis.Cmd, 0, end-start)
		for _, field := range fields[start:end] {
			lengthCommands = append(lengthCommands, lengthPipe.Do(ctx, "HSTRLEN", s.hash, field))
		}
		if _, err := lengthPipe.Exec(ctx); err != nil {
			return nil, fmt.Errorf("read redis hash field lengths: %w", err)
		}
		preflightBytes := materializedBytes
		for i, field := range fields[start:end] {
			length, err := lengthCommands[i].Int64()
			if err != nil {
				return nil, fmt.Errorf("read redis hash field %q length: %w", field, err)
			}
			preflightBytes += int64(len(field)) + length
			if preflightBytes > s.maxBytes {
				return nil, fmt.Errorf("redis configuration materialization exceeds %d bytes", s.maxBytes)
			}
		}

		valuePipe := s.client.Pipeline()
		valueCommands := make([]*redis.StringCmd, 0, end-start)
		for _, field := range fields[start:end] {
			valueCommands = append(valueCommands, valuePipe.HGet(ctx, s.hash, field))
		}
		if _, err := valuePipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
			return nil, fmt.Errorf("read redis hash fields: %w", err)
		}
		for i, field := range fields[start:end] {
			value, err := valueCommands[i].Result()
			if errors.Is(err, redis.Nil) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("read redis hash field %q: %w", field, err)
			}
			materializedBytes += int64(len(field) + len(value))
			if materializedBytes > s.maxBytes {
				return nil, fmt.Errorf("redis configuration materialization exceeds %d bytes", s.maxBytes)
			}
			values[field] = value
		}
	}
	return values, nil
}

func (s *RedisSource) hashFieldNames(ctx context.Context) ([]string, error) {
	fields, err := s.scanHashFieldNames(ctx)
	if err == nil {
		return fields, nil
	}
	if !redisHashScanWithoutValuesUnsupported(err) {
		return nil, fmt.Errorf("scan redis hash field names: %w", err)
	}

	// HSCAN NOVALUES was added after HSCAN. HKEYS keeps the compatibility path
	// value-free; values are still fetched later in bounded HSTRLEN/HGET batches.
	fields, err = s.client.HKeys(ctx, s.hash).Result()
	if err != nil {
		return nil, fmt.Errorf("list redis hash field names: %w", err)
	}
	if len(fields) > maxSourceEntries {
		return nil, fmt.Errorf("redis configuration exceeds %d entries", maxSourceEntries)
	}
	var fieldBytes int64
	for _, field := range fields {
		fieldBytes += int64(len(field))
		if fieldBytes > s.maxBytes {
			return nil, fmt.Errorf("redis configuration materialization exceeds %d bytes", s.maxBytes)
		}
	}
	sort.Strings(fields)
	return fields, nil
}

func (s *RedisSource) scanHashFieldNames(ctx context.Context) ([]string, error) {
	fields := make([]string, 0)
	var cursor uint64
	for {
		page, next, err := s.client.HScanNoValues(ctx, s.hash, cursor, "", 128).Result()
		if err != nil {
			return nil, err
		}
		if len(fields)+len(page) > maxSourceEntries {
			return nil, fmt.Errorf("redis configuration exceeds %d entries", maxSourceEntries)
		}
		for _, field := range page {
			if int64(len(field)) > s.maxBytes {
				return nil, fmt.Errorf("redis configuration materialization exceeds %d bytes", s.maxBytes)
			}
			fields = append(fields, field)
		}
		cursor = next
		if cursor == 0 {
			sort.Strings(fields)
			return fields, nil
		}
	}
}

func redisHashScanWithoutValuesUnsupported(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "unknown command") ||
		strings.Contains(message, "unknown subcommand") ||
		strings.Contains(message, "syntax error")
}

func (s *RedisSource) loadPrefix(ctx context.Context) (map[string]string, error) {
	for _, field := range completeConfigKeys {
		value, found, err := s.readStringIfPresent(ctx, s.prefix+field)
		if err != nil {
			return nil, err
		}
		if found {
			return map[string]string{field: value}, nil
		}
	}

	values := make(map[string]string)
	var cursor uint64
	var materializedBytes int64
	for {
		keys, next, err := s.client.Scan(ctx, cursor, s.prefix+"*", 128).Result()
		if err != nil {
			return nil, fmt.Errorf("scan redis config keys: %w", err)
		}
		sort.Strings(keys)
		if len(values)+len(keys) > maxSourceEntries {
			return nil, fmt.Errorf("redis configuration exceeds %d entries", maxSourceEntries)
		}
		for start := 0; start < len(keys); start += 128 {
			end := start + 128
			if end > len(keys) {
				end = len(keys)
			}
			lengthPipe := s.client.Pipeline()
			lengthCommands := make([]*redis.IntCmd, 0, end-start)
			for _, key := range keys[start:end] {
				lengthCommands = append(lengthCommands, lengthPipe.StrLen(ctx, key))
			}
			if _, err := lengthPipe.Exec(ctx); err != nil {
				return nil, fmt.Errorf("read redis config key lengths: %w", err)
			}
			preflightBytes := materializedBytes
			for i, key := range keys[start:end] {
				length, err := lengthCommands[i].Result()
				if err != nil {
					return nil, fmt.Errorf("read redis config key %q length: %w", key, err)
				}
				field := strings.TrimPrefix(key, s.prefix)
				preflightBytes += int64(len(field)) + length
				if preflightBytes > s.maxBytes {
					return nil, fmt.Errorf("redis configuration materialization exceeds %d bytes", s.maxBytes)
				}
			}

			pipe := s.client.Pipeline()
			commands := make([]*redis.StringCmd, 0, end-start)
			for _, key := range keys[start:end] {
				commands = append(commands, pipe.Get(ctx, key))
			}
			if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
				return nil, fmt.Errorf("read redis config keys: %w", err)
			}
			for i, key := range keys[start:end] {
				value, err := commands[i].Result()
				if errors.Is(err, redis.Nil) {
					continue
				}
				if err != nil {
					return nil, fmt.Errorf("read redis config key %q: %w", key, err)
				}
				field := strings.TrimPrefix(key, s.prefix)
				materializedBytes += int64(len(field) + len(value))
				if materializedBytes > s.maxBytes {
					return nil, fmt.Errorf("redis configuration materialization exceeds %d bytes", s.maxBytes)
				}
				values[field] = value
			}
		}
		cursor = next
		if cursor == 0 {
			return values, nil
		}
	}
}

func (s *RedisSource) readHashString(ctx context.Context, field string) (string, error) {
	length, err := s.client.Do(ctx, "HSTRLEN", s.hash, field).Int64()
	if err != nil {
		return "", fmt.Errorf("read redis hash field length: %w", err)
	}
	if length > s.maxBytes {
		return "", fmt.Errorf("redis configuration value exceeds %d bytes", s.maxBytes)
	}
	value, err := s.client.HGet(ctx, s.hash, field).Result()
	if err != nil {
		return "", fmt.Errorf("read redis hash field: %w", err)
	}
	if int64(len(value)) > s.maxBytes {
		return "", fmt.Errorf("redis configuration value exceeds %d bytes", s.maxBytes)
	}
	return value, nil
}

func (s *RedisSource) readStringIfPresent(ctx context.Context, key string) (string, bool, error) {
	exists, err := s.client.Exists(ctx, key).Result()
	if err != nil {
		return "", false, fmt.Errorf("check redis config key: %w", err)
	}
	if exists == 0 {
		return "", false, nil
	}
	value, err := s.readString(ctx, key)
	return value, true, err
}

func (s *RedisSource) readString(ctx context.Context, key string) (string, error) {
	length, err := s.client.StrLen(ctx, key).Result()
	if err != nil {
		return "", fmt.Errorf("read Redis value length: %w", err)
	}
	if length > s.maxBytes {
		return "", fmt.Errorf("redis configuration value exceeds %d bytes", s.maxBytes)
	}
	value, err := s.client.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read Redis value: %w", err)
	}
	if int64(len(value)) > s.maxBytes {
		return "", fmt.Errorf("redis configuration value exceeds %d bytes", s.maxBytes)
	}
	return value, nil
}

func (s *RedisSource) Close() error { return s.client.Close() }

func (s *RedisSource) Write(ctx context.Context, data []byte) error {
	_, err := s.client.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		if s.hash != "" {
			pipe.HSet(ctx, s.hash, "config.yaml", string(data))
		} else {
			pipe.Set(ctx, s.prefix+"config.yaml", data, 0)
		}
		if s.versionKey != "" {
			pipe.Incr(ctx, s.versionKey)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("write redis config transaction: %w", err)
	}
	return nil
}

func marshalDeterministic(value any) ([]byte, error) {
	// yaml.v3 sorts string map keys, producing stable bytes for hashing.
	b, err := yaml.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("marshal key snapshot: %w", err)
	}
	return b, nil
}
