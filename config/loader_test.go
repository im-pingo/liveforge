package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadRejectsRemovedStreamSetting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "liveforge.yaml")
	if err := os.WriteFile(path, []byte("stream:\n  audio_cache_ms: 1000\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Load(path)
	const want = "stream.audio_cache_ms has been removed; audio is interleaved in the GOP cache"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("removed setting error = %v, want %q", err, want)
	}
}
