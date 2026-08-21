package core

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ValidatePathWithinRoot checks both lexical and existing symlink resolution
// boundaries. It is intended for paths assembled from a configured directory
// and a validated stream key, before creating a file or directory.
func ValidatePathWithinRoot(root, candidate string) error {
	if strings.TrimSpace(root) == "" || strings.TrimSpace(candidate) == "" {
		return fmt.Errorf("path root and candidate must not be empty")
	}
	rootAbs, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return fmt.Errorf("resolve path root: %w", err)
	}
	candidateAbs, err := filepath.Abs(filepath.Clean(candidate))
	if err != nil {
		return fmt.Errorf("resolve candidate path: %w", err)
	}
	if !pathWithin(rootAbs, candidateAbs) {
		return fmt.Errorf("candidate path %q escapes root %q", candidate, root)
	}

	rootReal := existingRealPath(rootAbs)
	ancestor := candidateAbs
	for {
		if _, statErr := os.Lstat(ancestor); statErr == nil {
			resolved, resolveErr := filepath.EvalSymlinks(ancestor)
			if resolveErr != nil {
				return fmt.Errorf("resolve symlink path %q: %w", ancestor, resolveErr)
			}
			resolvedAbs, absErr := filepath.Abs(resolved)
			if absErr != nil {
				return fmt.Errorf("resolve symlink target %q: %w", ancestor, absErr)
			}
			if !pathWithin(rootReal, resolvedAbs) {
				return fmt.Errorf("candidate path %q resolves outside root %q", candidate, root)
			}
			break
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			break
		}
		ancestor = parent
	}
	return nil
}

func existingRealPath(path string) string {
	for current := path; ; current = filepath.Dir(current) {
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			if absolute, err := filepath.Abs(resolved); err == nil {
				return absolute
			}
		}
		parent := filepath.Dir(current)
		if parent == current {
			return path
		}
	}
}

func pathWithin(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
