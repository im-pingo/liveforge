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
