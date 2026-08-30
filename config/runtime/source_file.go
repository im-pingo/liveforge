package runtime

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// FileSourceOptions configures one local YAML/JSON document source.
type FileSourceOptions struct {
	Path     string
	MaxBytes int64
}

// FileSource reads one local YAML/JSON document.
type FileSource struct {
	path     string
	maxBytes int64
}

func NewFileSource(path string) (*FileSource, error) {
	return NewFileSourceWithOptions(FileSourceOptions{Path: path})
}

func NewFileSourceWithOptions(opts FileSourceOptions) (*FileSource, error) {
	if opts.Path == "" {
		return nil, fmt.Errorf("config file path is required")
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = DefaultSourceMaxBytes
	}
	return &FileSource{path: opts.Path, maxBytes: opts.MaxBytes}, nil
}

func (s *FileSource) Name() string { return "file" }

func (s *FileSource) Load(ctx context.Context, previous Version) (Snapshot, error) {
	select {
	case <-ctx.Done():
		return Snapshot{}, ctx.Err()
	default:
	}
	info, err := os.Stat(s.path)
	if err != nil {
		return Snapshot{}, fmt.Errorf("stat config file: %w", err)
	}
	if info.IsDir() {
		return Snapshot{}, fmt.Errorf("config path is a directory")
	}
	file, err := os.Open(s.path)
	if err != nil {
		return Snapshot{}, fmt.Errorf("open config file: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, s.maxBytes+1))
	if err != nil {
		return Snapshot{}, fmt.Errorf("read config file: %w", err)
	}
	if int64(len(data)) > s.maxBytes {
		return Snapshot{}, fmt.Errorf("config file exceeds %d bytes", s.maxBytes)
	}
	if len(data) == 0 {
		return Snapshot{}, fmt.Errorf("config file is empty")
	}
	return Snapshot{Data: append([]byte(nil), data...), Version: info.ModTime().UTC().Format(time.RFC3339Nano), LastModified: info.ModTime()}, nil
}

func (s *FileSource) Close() error { return nil }

func (s *FileSource) Write(ctx context.Context, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := os.Stat(s.path)
	mode := os.FileMode(0600)
	if err == nil {
		mode = info.Mode().Perm()
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat config file for write: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".liveforge-config-*")
	if err != nil {
		return fmt.Errorf("create config temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod config temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write config temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync config temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close config temp file: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("replace config file: %w", err)
	}
	return nil
}
