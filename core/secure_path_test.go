package core

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidatePathWithinRoot(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "live", "camera.ts")
	if err := ValidatePathWithinRoot(root, inside); err != nil {
		t.Fatalf("inside path rejected: %v", err)
	}
	if err := ValidatePathWithinRoot(root, filepath.Join(root, "..", "escape.ts")); err == nil {
		t.Fatal("path traversal was accepted")
	}

	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := ValidatePathWithinRoot(root, filepath.Join(root, "link", "escape.ts")); err == nil {
		t.Fatal("symlink escape was accepted")
	}
}
