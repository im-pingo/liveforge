package dvr

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/im-pingo/liveforge/internal/localfs"
)

type dvrStorage struct {
	root     *localfs.Root
	basePath string
	once     sync.Once
}

func newDVRStorage(pathTemplate string) (*dvrStorage, error) {
	basePath, err := filepath.Abs(dvrStorageRoot(pathTemplate))
	if err != nil {
		return nil, err
	}
	root, err := localfs.OpenRoot(basePath)
	if err != nil {
		return nil, fmt.Errorf("dvr: open storage root: %w", err)
	}
	return &dvrStorage{root: root, basePath: filepath.Clean(basePath)}, nil
}

func (s *dvrStorage) openStreamDir(pathTemplate, streamKey string) (*localfs.Dir, string, error) {
	target := resolvePath(pathTemplate, streamKey)
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return nil, "", err
	}
	rel, err := filepath.Rel(s.basePath, absTarget)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, "", fmt.Errorf("dvr: stream path escapes storage root")
	}
	dir, err := s.root.OpenDir(filepath.ToSlash(rel), true)
	if err != nil {
		return nil, "", fmt.Errorf("dvr: open stream directory: %w", err)
	}
	return dir, filepath.Clean(target), nil
}

func (s *dvrStorage) Close() error {
	var err error
	s.once.Do(func() { err = s.root.Close() })
	return err
}
