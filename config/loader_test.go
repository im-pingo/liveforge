package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
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

func TestLoadRejectsEnabledLLHLSNonPositiveSegmentDuration(t *testing.T) {
	for _, value := range []string{"0", "-0.1"} {
		t.Run(value, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "liveforge.yaml")
			document := "http_stream:\n  llhls:\n    enabled: true\n    segment_duration: " + value + "\n"
			if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "http_stream.llhls.segment_duration must be greater than zero") {
				t.Fatalf("Load segment_duration=%s error = %v", value, err)
			}
		})
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

func TestRemovedSettingTraversalBoundsRepeatedMergeDAG(t *testing.T) {
	benchmark := func(depth int) testing.BenchmarkResult {
		root := repeatedMergeDAG(t, depth)
		return testing.Benchmark(func(b *testing.B) {
			for range b.N {
				if yamlMappingContainsPath(
					root,
					[]string{"stream", "audio_cache_ms"},
					make(map[yamlTraversalState]bool),
				) {
					b.Fatal("unexpected removed setting in repeated merge DAG")
				}
			}
		})
	}

	shallow := benchmark(8)
	deep := benchmark(16)
	if deep.NsPerOp() > shallow.NsPerOp()*8 {
		t.Fatalf(
			"repeated merge DAG traversal growth = %d ns/op to %d ns/op, want <= 8x",
			shallow.NsPerOp(),
			deep.NsPerOp(),
		)
	}
}

func repeatedMergeDAG(t *testing.T, depth int) *yaml.Node {
	t.Helper()

	var source strings.Builder
	source.WriteString("level0: &level0 {}\n")
	for level := 1; level <= depth; level++ {
		fmt.Fprintf(&source, "level%d: &level%d\n  <<: [*level%d, *level%d]\n", level, level, level-1, level-1)
	}
	fmt.Fprintf(&source, "<<: *level%d\n", depth)

	var document yaml.Node
	if err := yaml.Unmarshal([]byte(source.String()), &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Content) != 1 {
		t.Fatalf("document content nodes = %d, want 1", len(document.Content))
	}
	return document.Content[0]
}
