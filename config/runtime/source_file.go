package runtime

import (
	"context"
	"fmt"
	"os"
	"time"
)

// FileSource reads one local YAML/JSON document.
type FileSource struct{ path string }

func NewFileSource(path string) (*FileSource, error) {
	if path == "" {
		return nil, fmt.Errorf("config file path is required")
	}
	return &FileSource{path: path}, nil
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
	data, err := os.ReadFile(s.path)
	if err != nil {
		return Snapshot{}, fmt.Errorf("read config file: %w", err)
	}
	if len(data) == 0 {
		return Snapshot{}, fmt.Errorf("config file is empty")
	}
	return Snapshot{Data: append([]byte(nil), data...), Version: info.ModTime().UTC().Format(time.RFC3339Nano), LastModified: info.ModTime()}, nil
}

func (s *FileSource) Close() error { return nil }
