package config

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

var namedEnvPattern = regexp.MustCompile(`\$\{[A-Za-z_][A-Za-z0-9_]*\}|\$[A-Za-z_][A-Za-z0-9_]*`)

// FileSource merges a user-owned base file with a machine-owned runtime
// override file. Only the override file is written by Store.
type FileSource struct {
	basePath     string
	overridePath string
	mu           sync.Mutex
}

func NewFileSource(basePath, overridePath string) *FileSource {
	return &FileSource{basePath: basePath, overridePath: overridePath}
}

// RuntimeOverridePath returns the sidecar path used for UI/runtime changes.
func RuntimeOverridePath(basePath string) string {
	ext := filepath.Ext(basePath)
	if ext == "" {
		return basePath + ".runtime.yaml"
	}
	return strings.TrimSuffix(basePath, ext) + ".runtime" + ext
}

func (s *FileSource) Name() string { return "file:" + s.basePath }

func (s *FileSource) Load(ctx context.Context) (Document, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked(ctx)
}

func (s *FileSource) loadLocked(ctx context.Context) (Document, error) {
	if err := ctx.Err(); err != nil {
		return Document{}, err
	}
	baseData, err := os.ReadFile(s.basePath)
	if err != nil {
		return Document{}, fmt.Errorf("read base config: %w", err)
	}
	base, err := decodeYAMLMap(baseData)
	if err != nil {
		return Document{}, fmt.Errorf("parse base config: %w", err)
	}

	override := map[string]any{}
	if s.overridePath != "" {
		overrideData, readErr := os.ReadFile(s.overridePath)
		switch {
		case readErr == nil:
			override, err = decodeYAMLMap(overrideData)
			if err != nil {
				return Document{}, fmt.Errorf("parse runtime override: %w", err)
			}
		case !errors.Is(readErr, os.ErrNotExist):
			return Document{}, fmt.Errorf("read runtime override: %w", readErr)
		}
	}

	merged := cloneMap(base)
	mergeMap(merged, override)
	cfg, canonical, err := decodeConfigMap(merged)
	if err != nil {
		return Document{}, err
	}
	return Document{
		Config:   cfg,
		Revision: revision(canonical),
		Source:   s.Name(),
	}, nil
}

func (s *FileSource) Store(ctx context.Context, patch Patch, expectedRevision string) (string, error) {
	if s.overridePath == "" {
		return "", ErrSourceReadOnly
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.storeLocked(ctx, patch, expectedRevision)
	if err != nil {
		return "", err
	}
	return doc.Revision, nil
}

// StoreAndApply keeps the source write and runtime acceptance transactional.
// The exact previous override is restored when apply rejects the candidate.
func (s *FileSource) StoreAndApply(ctx context.Context, patch Patch, expectedRevision string, apply func(Document) error) (string, error) {
	if s.overridePath == "" {
		return "", ErrSourceReadOnly
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	previousOverride, readErr := os.ReadFile(s.overridePath)
	previousExisted := readErr == nil
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return "", fmt.Errorf("read runtime override: %w", readErr)
	}
	doc, err := s.storeLocked(ctx, patch, expectedRevision)
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		if rollbackErr := s.restoreOverrideLocked(previousOverride, previousExisted); rollbackErr != nil {
			return "", fmt.Errorf("%w; restore runtime override: %v", err, rollbackErr)
		}
		return "", err
	}
	if apply == nil {
		return doc.Revision, nil
	}
	if err := apply(doc); err != nil {
		if rollbackErr := s.restoreOverrideLocked(previousOverride, previousExisted); rollbackErr != nil {
			return "", fmt.Errorf("%w; restore runtime override: %v", err, rollbackErr)
		}
		return "", err
	}
	return doc.Revision, nil
}

func (s *FileSource) storeLocked(ctx context.Context, patch Patch, expectedRevision string) (Document, error) {

	current, err := s.loadLocked(ctx)
	if err != nil {
		return Document{}, err
	}
	if expectedRevision != "" && current.Revision != expectedRevision {
		return Document{}, fmt.Errorf("%w: expected %s, current %s", ErrRevisionConflict, expectedRevision, current.Revision)
	}

	override := map[string]any{}
	data, readErr := os.ReadFile(s.overridePath)
	if readErr == nil {
		override, err = decodeYAMLMap(data)
		if err != nil {
			return Document{}, fmt.Errorf("parse runtime override: %w", err)
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return Document{}, fmt.Errorf("read runtime override: %w", readErr)
	}
	mergeMap(override, map[string]any(patch))

	baseData, err := os.ReadFile(s.basePath)
	if err != nil {
		return Document{}, fmt.Errorf("read base config: %w", err)
	}
	base, err := decodeYAMLMap(baseData)
	if err != nil {
		return Document{}, fmt.Errorf("parse base config: %w", err)
	}
	merged := cloneMap(base)
	mergeMap(merged, override)
	candidate, canonical, err := decodeConfigMap(merged)
	if err != nil {
		return Document{}, err
	}

	encoded, err := yaml.Marshal(override)
	if err != nil {
		return Document{}, fmt.Errorf("marshal runtime override: %w", err)
	}
	if err := writeAtomic(s.overridePath, encoded, 0o600); err != nil {
		return Document{}, err
	}
	return Document{Config: candidate, Revision: revision(canonical), Source: s.Name()}, nil
}

func (s *FileSource) restoreOverrideLocked(data []byte, existed bool) error {
	if existed {
		return writeAtomic(s.overridePath, data, 0o600)
	}
	if err := os.Remove(s.overridePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func decodeYAMLMap(data []byte) (map[string]any, error) {
	if len(data) == 0 {
		return map[string]any{}, nil
	}
	var value map[string]any
	if err := yaml.Unmarshal(data, &value); err != nil {
		return nil, err
	}
	if value == nil {
		value = map[string]any{}
	}
	return value, nil
}

func decodeConfigMap(value map[string]any) (*Config, []byte, error) {
	expanded := expandEnvLeaves(value).(map[string]any)
	canonical, err := yaml.Marshal(expanded)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal merged config: %w", err)
	}
	cfg := defaults()
	if err := yaml.Unmarshal(canonical, cfg); err != nil {
		return nil, nil, fmt.Errorf("parse merged config: %w", err)
	}
	normalize(cfg)
	if err := Validate(cfg); err != nil {
		return nil, nil, err
	}
	validated, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal validated config: %w", err)
	}
	return cfg, validated, nil
}

func expandEnvLeaves(value any) any {
	switch typed := value.(type) {
	case string:
		return expandEnvString(typed)
	case map[string]any:
		expanded := make(map[string]any, len(typed))
		for key, item := range typed {
			expanded[key] = expandEnvLeaves(item)
		}
		return expanded
	case []any:
		expanded := make([]any, len(typed))
		for i, item := range typed {
			expanded[i] = expandEnvLeaves(item)
		}
		return expanded
	default:
		return value
	}
}

func expandEnvString(value string) string {
	if strings.HasPrefix(value, "$2a$") || strings.HasPrefix(value, "$2b$") || strings.HasPrefix(value, "$2y$") {
		return value
	}
	return namedEnvPattern.ReplaceAllStringFunc(value, func(token string) string {
		name := token[1:]
		if strings.HasPrefix(token, "${") {
			name = token[2 : len(token)-1]
		}
		return os.Getenv(name)
	})
}

func mergeMap(dst, src map[string]any) {
	for key, value := range src {
		if value == nil {
			delete(dst, key)
			continue
		}
		srcMap, srcOK := value.(map[string]any)
		if !srcOK {
			dst[key] = value
			continue
		}
		dstMap, dstOK := dst[key].(map[string]any)
		if !dstOK {
			dstMap = map[string]any{}
			dst[key] = dstMap
		}
		mergeMap(dstMap, srcMap)
	}
}

func cloneMap(src map[string]any) map[string]any {
	dst := make(map[string]any, len(src))
	for key, value := range src {
		if nested, ok := value.(map[string]any); ok {
			dst[key] = cloneMap(nested)
		} else {
			dst[key] = value
		}
	}
	return dst
}

func revision(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create override directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".liveforge-config-*")
	if err != nil {
		return fmt.Errorf("create runtime override: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod runtime override: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write runtime override: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync runtime override: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close runtime override: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace runtime override: %w", err)
	}
	return nil
}
