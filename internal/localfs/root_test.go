package localfs

import (
	"errors"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func rejectHardLinks(t *testing.T) {
	t.Helper()
	original := linkAt
	linkAt = func(int, string, int, string, int) error { return unix.EOPNOTSUPP }
	t.Cleanup(func() { linkAt = original })
}

func TestOpenRootRejectsUnsupportedHardLinksWithoutProbeArtifacts(t *testing.T) {
	path := t.TempDir()
	rejectHardLinks(t)
	root, err := OpenRoot(path)
	if root != nil {
		_ = root.Close()
		t.Fatal("OpenRoot returned a boundary without hard-link support")
	}
	if !errors.Is(err, ErrHardLinksUnsupported) {
		t.Fatalf("OpenRoot error = %v, want ErrHardLinksUnsupported", err)
	}
	entries, readErr := os.ReadDir(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("hard-link probe artifacts remain: %v", entries)
	}
}

func TestPendingPublishAndPreserveRetainSourceWhenHardLinksDisappear(t *testing.T) {
	root, err := OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	pending, err := root.CreatePending("live/segment.ts.partial", 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer pending.Close()
	if _, err := pending.File.Write([]byte("pending")); err != nil {
		t.Fatal(err)
	}
	if err := pending.File.Close(); err != nil {
		t.Fatal(err)
	}
	rejectHardLinks(t)

	if err := pending.PublishAs("segment.ts"); !errors.Is(err, ErrHardLinksUnsupported) {
		t.Fatalf("PublishAs error = %v, want ErrHardLinksUnsupported", err)
	}
	if _, _, err := root.Open("live/segment.ts"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("final object exists after failed publish: %v", err)
	}
	if data, err := root.ReadFile("live/segment.ts.partial"); err != nil || string(data) != "pending" {
		t.Fatalf("pending data = %q, err=%v", data, err)
	}

	if _, err := pending.PreserveAs(func(int) string { return "segment.ts.failed" }); !errors.Is(err, ErrHardLinksUnsupported) {
		t.Fatalf("PreserveAs error = %v, want ErrHardLinksUnsupported", err)
	}
	if data, err := root.ReadFile("live/segment.ts.partial"); err != nil || string(data) != "pending" {
		t.Fatalf("pending data after preserve = %q, err=%v", data, err)
	}
}

func TestRecoveryMoveRetainsCrashPartialWhenHardLinksDisappear(t *testing.T) {
	root, err := OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	pending, err := root.CreatePending("live/crash.partial", 0644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pending.File.Write([]byte("crash")); err != nil {
		t.Fatal(err)
	}
	if err := pending.File.Close(); err != nil {
		t.Fatal(err)
	}
	if err := pending.Close(); err != nil {
		t.Fatal(err)
	}
	rejectHardLinks(t)

	if _, err := root.MoveToUnique("live/crash.partial", func(int) string { return "crash.failed" }); !errors.Is(err, ErrHardLinksUnsupported) {
		t.Fatalf("recovery move error = %v, want ErrHardLinksUnsupported", err)
	}
	if data, err := root.ReadFile("live/crash.partial"); err != nil || string(data) != "crash" {
		t.Fatalf("crash partial data = %q, err=%v", data, err)
	}
}
