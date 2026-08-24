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
