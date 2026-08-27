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

func TestLoadRejectsRemovedStreamSettingThroughYAMLIndirection(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("testdata", "removed-settings", "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("no removed-setting fixtures found")
	}

	for _, path := range paths {
		path := path
		t.Run(strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)), func(t *testing.T) {
			_, err := Load(path)
			const want = "stream.audio_cache_ms has been removed; audio is interleaved in the GOP cache"
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("removed setting error = %v, want %q", err, want)
			}
		})
	}
}
