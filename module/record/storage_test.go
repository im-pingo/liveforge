package record

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
)

func TestLocalStorageLifecycle(t *testing.T) {
	storage, err := NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	object, err := storage.Create(context.Background(), "live/cam/record.flv", RecordingInfo{
		StreamKey: "live/cam",
		Format:    "flv",
		StartedAt: time.Unix(100, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := object.Write([]byte("recording")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(storage.Root(), "live/cam/record.flv")); !os.IsNotExist(err) {
		t.Fatalf("final file visible before completion: %v", err)
	}

	completed, err := object.Complete(context.Background(), RecordingInfo{DurationSec: time.Second.Seconds()})
	if err != nil {
		t.Fatal(err)
	}
	if completed.ID != "live/cam/record.flv" || completed.State != RecordingCompleted || completed.Size != 9 {
		t.Fatalf("completed = %+v", completed)
	}

	items, err := storage.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != completed.ID {
		t.Fatalf("list = %+v", items)
	}
	got, err := storage.Stat(context.Background(), completed.ID)
	if err != nil || got.StreamKey != "live/cam" {
		t.Fatalf("stat = %+v, %v", got, err)
	}

	reader, info, err := storage.Open(context.Background(), completed.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil || string(data) != "recording" || info.ID != completed.ID {
		t.Fatalf("open data=%q info=%+v err=%v", data, info, err)
	}

	if err := storage.Delete(context.Background(), completed.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.Stat(context.Background(), completed.ID); !errors.Is(err, ErrRecordingNotFound) {
		t.Fatalf("stat after delete = %v", err)
	}
}

func TestLocalStorageDeleteRemovesOnlyOwnedTSSidecars(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "live", "cam")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"record.ts":                  "primary",
		"record.ts.segment_00000.ts": "segment",
		"record.ts.segment_00001.ts": "segment",
		"record.ts.m3u8":             "playlist",
		"other.ts":                   "other primary",
		"other.ts.segment_00000.ts":  "other segment",
		"other.ts.m3u8":              "other playlist",
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	storage, err := NewLocalStorage(root)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()

	if err := storage.Delete(context.Background(), "live/cam/record.ts"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"record.ts", "record.ts.segment_00000.ts", "record.ts.segment_00001.ts", "record.ts.m3u8"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("owned TS artifact %q still exists: %v", name, err)
		}
	}
	for _, name := range []string{"other.ts", "other.ts.segment_00000.ts", "other.ts.m3u8"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(data) != files[name] {
			t.Fatalf("unrelated TS artifact %q changed: data=%q err=%v", name, data, err)
		}
	}
}

func TestLocalStorageListsAndPreservesRecordingWithTSSidecarLikeName(t *testing.T) {
	root := t.TempDir()
	storage, err := NewLocalStorage(root)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()

	completeRecording := func(id, streamKey, data string) {
		t.Helper()
		object, createErr := storage.Create(context.Background(), id, RecordingInfo{
			StreamKey: streamKey,
			Format:    "ts",
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		if _, writeErr := object.Write([]byte(data)); writeErr != nil {
			t.Fatal(writeErr)
		}
		if _, completeErr := object.Complete(context.Background(), RecordingInfo{}); completeErr != nil {
			t.Fatal(completeErr)
		}
	}

	completeRecording("live/cam/record.ts", "live/cam", "owner")
	lookalikeID := "live/cam/record.ts.segment_00000.ts.notes"
	completeRecording(lookalikeID, "live/notes", "independent")

	dir := filepath.Join(root, "live", "cam")
	ownedArtifacts := []string{
		"record.ts.segment_00000.ts",
		"record.ts.segment_00000.ts.partial",
		"record.ts.segment_00001.ts.failed",
		"record.ts.segment_00002.ts.orphan-100-2.failed",
		"record.ts.m3u8",
		"record.ts.m3u8.partial",
		"record.ts.m3u8.failed",
		"record.ts.m3u8.orphan-101-3.failed",
	}
	for _, name := range ownedArtifacts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("sidecar"), 0600); err != nil {
			t.Fatal(err)
		}
	}

	items, err := storage.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	listed := make(map[string]bool, len(items))
	for _, item := range items {
		listed[item.ID] = true
	}
	for _, id := range []string{"live/cam/record.ts", lookalikeID} {
		if !listed[id] {
			t.Errorf("recording %q missing from list: %+v", id, items)
		}
	}

	if err := storage.Delete(context.Background(), "live/cam/record.ts"); err != nil {
		t.Fatal(err)
	}
	for _, name := range ownedArtifacts {
		if _, statErr := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(statErr) {
			t.Errorf("owned TS artifact %q still exists: %v", name, statErr)
		}
	}
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(lookalikeID)))
	if err != nil || string(data) != "independent" {
		t.Errorf("lookalike recording data=%q err=%v", data, err)
	}
	info, err := storage.Stat(context.Background(), lookalikeID)
	if err != nil || info.StreamKey != "live/notes" {
		t.Errorf("lookalike recording metadata=%+v err=%v", info, err)
	}
}

func TestLocalStorageDeleteCleanupFailureLeavesPrimaryForRetry(t *testing.T) {
	root := t.TempDir()
	storage, err := NewLocalStorage(root)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()

	object, err := storage.Create(context.Background(), "live/cam/record.ts", RecordingInfo{Format: "ts"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := object.Write([]byte("primary")); err != nil {
		t.Fatal(err)
	}
	sidecar := object.(sidecarMediaFile)
	segment, err := sidecar.CreateSidecar("record.ts.segment_00000.ts", 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := segment.Write([]byte("segment")); err != nil {
		t.Fatal(err)
	}
	if err := segment.Complete(); err != nil {
		t.Fatal(err)
	}
	if _, err := object.Complete(context.Background(), RecordingInfo{}); err != nil {
		t.Fatal(err)
	}

	primary := filepath.Join(root, "live", "cam", "record.ts")
	metadata := primary + metadataSuffix
	if err := os.Remove(metadata); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(metadata, 0700); err != nil {
		t.Fatal(err)
	}

	if err := storage.Delete(context.Background(), "live/cam/record.ts"); err == nil {
		t.Fatal("delete succeeded despite metadata cleanup failure")
	}
	if data, err := os.ReadFile(primary); err != nil || string(data) != "primary" {
		t.Fatalf("primary was not preserved after cleanup failure: data=%q err=%v", data, err)
	}

	if err := os.Remove(metadata); err != nil {
		t.Fatal(err)
	}
	if err := storage.Delete(context.Background(), "live/cam/record.ts"); err != nil {
		t.Fatalf("retry delete: %v", err)
	}
	if _, err := os.Stat(primary); !os.IsNotExist(err) {
		t.Fatalf("primary still exists after retry: %v", err)
	}
}

func TestLocalStorageDeleteRejectsOwnedNonRegularSidecarsBeforePrimary(t *testing.T) {
	tests := []struct {
		name    string
		replace func(*testing.T, string) string
		verify  func(*testing.T, string, string)
	}{
		{
			name: "symlink",
			replace: func(t *testing.T, artifact string) string {
				t.Helper()
				target := filepath.Join(t.TempDir(), "outside.ts")
				if err := os.WriteFile(target, []byte("outside"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(artifact); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, artifact); err != nil {
					t.Fatal(err)
				}
				return target
			},
			verify: func(t *testing.T, artifact, target string) {
				t.Helper()
				info, err := os.Lstat(artifact)
				if err != nil || info.Mode()&os.ModeSymlink == 0 {
					t.Fatalf("owned symlink info=%v err=%v", info, err)
				}
				if data, err := os.ReadFile(target); err != nil || string(data) != "outside" {
					t.Fatalf("symlink target data=%q err=%v", data, err)
				}
			},
		},
		{
			name: "directory",
			replace: func(t *testing.T, artifact string) string {
				t.Helper()
				if err := os.Remove(artifact); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(artifact, 0700); err != nil {
					t.Fatal(err)
				}
				return ""
			},
			verify: func(t *testing.T, artifact, _ string) {
				t.Helper()
				info, err := os.Lstat(artifact)
				if err != nil || !info.IsDir() {
					t.Fatalf("owned directory info=%v err=%v", info, err)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			storage, err := NewLocalStorage(root)
			if err != nil {
				t.Fatal(err)
			}
			defer storage.Close()

			object, err := storage.Create(context.Background(), "live/cam/record.ts", RecordingInfo{Format: "ts"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := object.Write([]byte("primary")); err != nil {
				t.Fatal(err)
			}
			sidecars := object.(sidecarMediaFile)
			segment, err := sidecars.CreateSidecar("record.ts.segment_00000.ts", 0600)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := segment.Write([]byte("segment")); err != nil {
				t.Fatal(err)
			}
			if err := segment.Complete(); err != nil {
				t.Fatal(err)
			}
			if _, err := object.Complete(context.Background(), RecordingInfo{}); err != nil {
				t.Fatal(err)
			}

			primary := filepath.Join(root, "live", "cam", "record.ts")
			metadata := primary + metadataSuffix
			artifact := primary + ".segment_00000.ts"
			target := test.replace(t, artifact)

			if err := storage.Delete(context.Background(), "live/cam/record.ts"); err == nil {
				t.Fatal("delete succeeded despite exact-owned non-regular sidecar")
			}
			if data, err := os.ReadFile(primary); err != nil || string(data) != "primary" {
				t.Fatalf("primary after rejected cleanup data=%q err=%v", data, err)
			}
			if info, err := os.Stat(metadata); err != nil || !info.Mode().IsRegular() {
				t.Fatalf("metadata after rejected cleanup info=%v err=%v", info, err)
			}
			test.verify(t, artifact, target)

			if err := os.Remove(artifact); err != nil {
				t.Fatal(err)
			}
			if err := storage.Delete(context.Background(), "live/cam/record.ts"); err != nil {
				t.Fatalf("retry delete after repairing obstruction: %v", err)
			}
			for _, path := range []string{primary, metadata} {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatalf("artifact %q remains after retry: %v", path, err)
				}
			}
		})
	}
}

func TestLocalStorageDeleteFailedTSRemovesOriginalSidecars(t *testing.T) {
	root := t.TempDir()
	storage, err := NewLocalStorage(root)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	object, err := storage.Create(context.Background(), "live/cam/record.ts", RecordingInfo{Format: "ts"})
	if err != nil {
		t.Fatal(err)
	}
	if _, writeErr := object.Write([]byte("primary")); writeErr != nil {
		t.Fatal(writeErr)
	}
	sidecars := object.(sidecarMediaFile)
	segment, err := sidecars.CreateSidecar("record.ts.segment_00000.ts", 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, writeErr := segment.Write([]byte("segment")); writeErr != nil {
		t.Fatal(writeErr)
	}
	if completeErr := segment.Complete(); completeErr != nil {
		t.Fatal(completeErr)
	}
	if writeErr := sidecars.WriteSidecarAtomic("record.ts.m3u8", []byte("#EXTM3U\n"), 0600); writeErr != nil {
		t.Fatal(writeErr)
	}
	failed, err := object.Fail(context.Background(), errors.New("injected failure"))
	if err != nil {
		t.Fatal(err)
	}
	if failed.ID != "live/cam/record.ts.failed" {
		t.Fatalf("failed recording ID = %q", failed.ID)
	}

	if err := storage.Delete(context.Background(), failed.ID); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"record.ts.failed", "record.ts.segment_00000.ts", "record.ts.m3u8"} {
		if _, statErr := os.Stat(filepath.Join(root, "live", "cam", name)); !os.IsNotExist(statErr) {
			t.Fatalf("failed TS artifact %q still exists: %v", name, statErr)
		}
	}
}

func TestLocalStorageRejectsTraversal(t *testing.T) {
	storage, err := NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{"../outside.flv", "/tmp/outside.flv", "live/../../outside.flv", ".", ""} {
		if _, err := storage.Stat(context.Background(), id); !errors.Is(err, ErrInvalidRecordingID) {
			t.Errorf("Stat(%q) error = %v", id, err)
		}
		if err := storage.Delete(context.Background(), id); !errors.Is(err, ErrInvalidRecordingID) {
			t.Errorf("Delete(%q) error = %v", id, err)
		}
	}
}

func TestLocalStoragePreservesFailedPartial(t *testing.T) {
	storage, err := NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	object, err := storage.Create(context.Background(), "live/cam/broken.flv", RecordingInfo{StreamKey: "live/cam"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := object.Write([]byte("partial")); err != nil {
		t.Fatal(err)
	}

	failed, err := object.Fail(context.Background(), errors.New("disk full"))
	if err != nil {
		t.Fatal(err)
	}
	if failed.State != RecordingFailed || failed.Error != "disk full" || failed.Size != 7 {
		t.Fatalf("failed = %+v", failed)
	}
	if filepath.Ext(failed.ID) != ".failed" {
		t.Fatalf("failed id = %q", failed.ID)
	}
	if _, err := storage.Stat(context.Background(), failed.ID); err != nil {
		t.Fatal(err)
	}
}

func TestLocalStorageHealthReportsCapacity(t *testing.T) {
	storage, err := NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	health := storage.Health(context.Background())
	if !health.Healthy || health.TotalBytes == 0 || health.AvailableBytes == 0 {
		t.Fatalf("health = %+v", health)
	}
}

func TestStorageTemplateUsesConfiguredFormat(t *testing.T) {
	for _, test := range []struct {
		format string
		ext    string
	}{
		{format: "flv", ext: ".flv"},
		{format: "mp4", ext: ".mp4"},
		{format: "fmp4", ext: ".mp4"},
		{format: "ts", ext: ".ts"},
	} {
		storage, template, err := newStorageForConfig(config.RecordConfig{Format: test.format, Path: filepath.Join(t.TempDir(), "{stream_key}", "{date}_{time}"+test.ext)})
		if err != nil {
			t.Fatal(err)
		}
		if storage.Root() == "" || filepath.Ext(template) != test.ext {
			t.Fatalf("format=%q root=%q template=%q", test.format, storage.Root(), template)
		}
	}
}

func TestStorageTemplateDefaultsToMP4WhenFormatIsUnset(t *testing.T) {
	t.Chdir(t.TempDir())
	storage, template, err := newStorageForConfig(config.RecordConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	if filepath.Ext(template) != ".mp4" {
		t.Fatalf("default recording template = %q, want .mp4", template)
	}
}

func TestNewFileWriterValidatesFormatAndMaxSize(t *testing.T) {
	for _, test := range []struct {
		name string
		cfg  config.RecordConfig
	}{
		{name: "format", cfg: config.RecordConfig{Format: "webm"}},
		{name: "max size", cfg: config.RecordConfig{Format: "flv", Segment: config.SegmentConfig{MaxSize: "1.5MB"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			if _, err := NewFileWriter("live/invalid", test.cfg); err == nil {
				t.Fatal("NewFileWriter accepted invalid recording configuration")
			}
		})
	}
}

func TestHLSRecordFormatUsesTSStorageSemantics(t *testing.T) {
	storage, template, err := newStorageForConfig(config.RecordConfig{Format: "hls"})
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	if filepath.Ext(template) != ".ts" {
		t.Fatalf("hls recording template = %q, want .ts", template)
	}
	w, err := newFileWriterWithStorage("live/hls-alias", config.RecordConfig{Format: "hls"}, storage, "live/hls-alias.ts", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if got := w.Format(); got != "ts" {
		t.Fatalf("hls writer format = %q, want canonical ts", got)
	}
}

func TestLocalStorageRecoversCrashPartialWithoutOverwritingFailure(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "live/cam")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	partial := filepath.Join(dir, "crash.flv.partial")
	if err := os.WriteFile(partial, []byte("crash bytes"), 0644); err != nil {
		t.Fatal(err)
	}
	existingFailed := filepath.Join(dir, "crash.flv.failed")
	if err := os.WriteFile(existingFailed, []byte("older failure"), 0644); err != nil {
		t.Fatal(err)
	}

	storage, err := NewLocalStorage(root)
	if err != nil {
		t.Fatal(err)
	}
	items, err := storage.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("recovered items = %+v", items)
	}
	if data, err := os.ReadFile(existingFailed); err != nil || string(data) != "older failure" {
		t.Fatalf("existing failure data=%q err=%v", data, err)
	}
	if _, err := os.Stat(partial); !os.IsNotExist(err) {
		t.Fatalf("partial still visible: %v", err)
	}
	var recovered RecordingInfo
	for _, item := range items {
		if item.ID != "live/cam/crash.flv.failed" {
			recovered = item
		}
	}
	if recovered.State != RecordingFailed || recovered.Error == "" || recovered.Size != int64(len("crash bytes")) {
		t.Fatalf("recovered = %+v", recovered)
	}
	reader, _, err := storage.Open(context.Background(), recovered.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil || string(data) != "crash bytes" {
		t.Fatalf("recovered data=%q err=%v", data, err)
	}
}

func TestLocalStorageRecoversTSPartialWithoutListingSidecarAsRecording(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "live", "cam")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	partial := filepath.Join(dir, "segment_00000.ts.partial")
	if err := os.WriteFile(partial, []byte("partial segment"), 0600); err != nil {
		t.Fatal(err)
	}

	storage, err := NewLocalStorage(root)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	items, err := storage.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("recovered TS sidecar leaked into recording list: %+v", items)
	}
	if _, statErr := os.Stat(partial); !os.IsNotExist(statErr) {
		t.Fatalf("TS sidecar partial still visible: %v", statErr)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "segment_00000.ts*.failed"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("recovered TS sidecar failures = %v, want one", matches)
	}
}

func TestLocalStorageCreateDoesNotFollowIntermediateSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	storage, err := NewLocalStorage(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "live")); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.Create(context.Background(), "live/cam/file.flv", RecordingInfo{}); !errors.Is(err, ErrInvalidRecordingID) {
		t.Fatalf("create error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "cam")); !os.IsNotExist(err) {
		t.Fatalf("outside directory was created before rejection: %v", err)
	}
}

func TestLocalStoragePendingFinalizePinsOriginalDirectory(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	storage, err := NewLocalStorage(root)
	if err != nil {
		t.Fatal(err)
	}
	object, err := storage.Create(context.Background(), "live/cam/file.flv", RecordingInfo{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := object.Write([]byte("safe")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "live"), filepath.Join(root, "moved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "live")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(outside, "cam"), 0755); err != nil {
		t.Fatal(err)
	}
	attackPartial := filepath.Join(outside, "cam/file.flv.partial")
	if err := os.WriteFile(attackPartial, []byte("attack"), 0644); err != nil {
		t.Fatal(err)
	}

	if _, err := object.Complete(context.Background(), RecordingInfo{}); err != nil {
		t.Fatal(err)
	}
	safeData, err := os.ReadFile(filepath.Join(root, "moved/cam/file.flv"))
	if err != nil || string(safeData) != "safe" {
		t.Fatalf("safe data=%q err=%v", safeData, err)
	}
	attackData, err := os.ReadFile(attackPartial)
	if err != nil || string(attackData) != "attack" {
		t.Fatalf("outside partial data=%q err=%v", attackData, err)
	}
	if _, err := os.Stat(filepath.Join(outside, "cam/file.flv")); !os.IsNotExist(err) {
		t.Fatalf("outside final created: %v", err)
	}
}

func TestStandaloneFileWriterClosesPrivateStorageBoundary(t *testing.T) {
	cfg := config.RecordConfig{
		Format: "flv",
		Path:   filepath.Join(t.TempDir(), "{stream_key}.flv"),
	}
	writer, err := NewFileWriter("live/private", cfg)
	if err != nil {
		t.Fatal(err)
	}
	storage, ok := writer.storage.(*LocalStorage)
	if !ok {
		t.Fatalf("storage type = %T", writer.storage)
	}
	writer.Close()
	if _, err := storage.List(context.Background()); err == nil {
		t.Fatal("standalone writer left its private storage boundary open")
	}
}
