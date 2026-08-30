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

func TestEntryFileModeAcceptsNamedFileMode(t *testing.T) {
	mode := os.FileMode(unix.S_IFDIR | 0o750)
	got := entryFileMode(mode)
	if !got.IsDir() || got.Perm() != 0o750 {
		t.Fatalf("entryFileMode(%#o) = %#o, want directory mode 0750", mode, got)
	}
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

func TestWriteSiblingAtomicClassifiesOnlyUnsupportedLinkErrors(t *testing.T) {
	tests := []struct {
		name        string
		stage       string
		errno       error
		unsupported bool
	}{
		{name: "write no space", stage: "write", errno: unix.ENOSPC},
		{name: "sync io", stage: "sync", errno: unix.EIO},
		{name: "close io", stage: "close", errno: unix.EIO},
		{name: "link permission", stage: "link", errno: unix.EPERM},
		{name: "link access", stage: "link", errno: unix.EACCES},
		{name: "link count", stage: "link", errno: unix.EMLINK},
		{name: "link no space", stage: "link", errno: unix.ENOSPC},
		{name: "link io", stage: "link", errno: unix.EIO},
		{name: "link operation unsupported", stage: "link", errno: unix.EOPNOTSUPP, unsupported: true},
		{name: "link syscall unavailable", stage: "link", errno: unix.ENOSYS, unsupported: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := t.TempDir()
			root, err := OpenRoot(path)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()

			originalWrite := writeFile
			originalSync := syncFile
			originalClose := closeFile
			originalLink := linkAt
			t.Cleanup(func() {
				writeFile = originalWrite
				syncFile = originalSync
				closeFile = originalClose
				linkAt = originalLink
			})
			switch test.stage {
			case "write":
				writeFile = func(*os.File, []byte) (int, error) { return 0, test.errno }
			case "sync":
				syncFile = func(*os.File) error { return test.errno }
			case "close":
				closeFile = func(file *os.File) error {
					_ = file.Close()
					return test.errno
				}
			case "link":
				linkAt = func(int, string, int, string, int) error { return test.errno }
			}

			err = root.WriteFileAtomic("target.json", []byte("payload"), 0o644)
			if !errors.Is(err, test.errno) {
				t.Fatalf("error = %v, want preserved cause %v", err, test.errno)
			}
			if got := errors.Is(err, ErrHardLinksUnsupported); got != test.unsupported {
				t.Fatalf("ErrHardLinksUnsupported = %v, want %v: %v", got, test.unsupported, err)
			}
			entries, readErr := os.ReadDir(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(entries) != 0 {
				t.Fatalf("failed atomic write left artifacts: %v", entries)
			}
		})
	}
}
