package loopexec

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

func fixtureRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	gitCmd(t, root, "init", "-q")
	gitCmd(t, root, "config", "user.email", "loop-test@example.invalid")
	gitCmd(t, root, "config", "user.name", "Loop Tests")
	writeFixture(t, root, ".gitignore", "ignored/\n.lf-loop/\n")
	writeFixture(t, root, "a.txt", "one")
	gitCmd(t, root, "add", ".")
	gitCmd(t, root, "commit", "-qm", "fixture")
	return root
}
func gitCmd(t *testing.T, root string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %s %v", args, out, err)
	}
	return strings.TrimSpace(string(out))
}
func writeFixture(t *testing.T, root, path, content string) {
	t.Helper()
	name := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}
func snapshot(t *testing.T, root string, inputs ...string) Snapshot {
	t.Helper()
	s, err := CaptureSnapshot(context.Background(), root, inputs)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func TestSnapshotsTrackFinalContentIndexAndInputs(t *testing.T) {
	root := fixtureRepo(t)
	first := snapshot(t, root)
	if first.Digest == "" || first.Head == "" {
		t.Fatal("snapshot omitted source identity")
	}
	if snapshot(t, root) != first {
		t.Fatal("stable source changed digest")
	}
	writeFixture(t, root, "a.txt", "two")
	dirty := snapshot(t, root)
	if dirty.Digest == first.Digest {
		t.Fatal("unstaged edit invisible")
	}
	gitCmd(t, root, "add", "a.txt")
	staged := snapshot(t, root)
	if staged.Digest == dirty.Digest {
		t.Fatal("index edit invisible")
	}
	writeFixture(t, root, "new.txt", "untracked")
	added := snapshot(t, root)
	if added.Digest == staged.Digest {
		t.Fatal("untracked input invisible")
	}
	if err := os.Remove(filepath.Join(root, "a.txt")); err != nil {
		t.Fatal(err)
	}
	deleted := snapshot(t, root)
	if deleted.Digest == added.Digest {
		t.Fatal("deletion invisible")
	}
	writeFixture(t, root, "ignored/fixture.bin", "first")
	if snapshot(t, root).Digest != deleted.Digest {
		t.Fatal("ignored outputs polluted source digest")
	}
	input := snapshot(t, root, "ignored/fixture.bin")
	writeFixture(t, root, "ignored/fixture.bin", "second")
	if snapshot(t, root, "ignored/fixture.bin").Digest == input.Digest {
		t.Fatal("explicit fixture mutation invisible")
	}
	if _, err := CaptureSnapshot(context.Background(), root, []string{"missing.bin"}); err == nil {
		t.Fatal("missing explicit input accepted")
	}
}
func TestSnapshotIncludesSymlinkIdentity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink privilege varies")
	}
	root := fixtureRepo(t)
	path := filepath.Join(root, "link")
	if err := os.Symlink("a.txt", path); err != nil {
		t.Fatal(err)
	}
	first := snapshot(t, root)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("other", path); err != nil {
		t.Fatal(err)
	}
	if snapshot(t, root).Digest == first.Digest {
		t.Fatal("symlink target invisible")
	}
}
func helperArgs(t *testing.T, mode, path string) []string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return []string{exe, "-test.run=^TestProcessHelper$", "--", "--lf-loop-helper", mode, path}
}
func TestProcessHelper(t *testing.T) {
	i := 0
	for i < len(os.Args) && os.Args[i] != "--lf-loop-helper" {
		i++
	}
	if i == len(os.Args) {
		return
	}
	mode, path := os.Args[i+1], os.Args[i+2]
	switch mode {
	case "signal":
		_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
		time.Sleep(time.Second)
		os.Exit(9)
	case "fake-env":
		_, _ = io.WriteString(os.Stdout, "required browser does not advertise H.264; You have not agreed to the Xcode license")
		os.Exit(1)
	case "collide":
		entries, err := os.ReadDir(path)
		if err != nil {
			os.Exit(9)
		}
		for _, entry := range entries {
			if entry.IsDir() {
				_ = os.WriteFile(filepath.Join(path, entry.Name(), "later.stdout.log"), []byte("collision"), 0600)
			}
		}
		os.Exit(0)
	case "exit":
		_, _ = io.WriteString(os.Stdout, "captured output")
		os.Exit(7)
	case "mark":
		_ = os.WriteFile(path, []byte("ran"), 0600)
		os.Exit(0)
	case "delayed":
		time.Sleep(600 * time.Millisecond)
		_ = os.WriteFile(path, []byte("orphan survived"), 0600)
		os.Exit(0)
	case "tree":
		cmd := exec.Command(os.Args[0], "-test.run=^TestProcessHelper$", "--", "--lf-loop-helper", "delayed", path)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			os.Exit(9)
		}
		_, _ = io.WriteString(os.Stdout, "child started\n")
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	os.Exit(8)
}
func TestProcessPreservesExitAndPreCanceledDoesNotStart(t *testing.T) {
	var output bytes.Buffer
	got := runProcess(context.Background(), t.TempDir(), helperArgs(t, "exit", ""), os.Environ(), &output, io.Discard)
	if got.ExitCode != 7 || !strings.Contains(output.String(), "captured output") {
		t.Fatalf("exit=%+v output=%s", got, output.String())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	marker := filepath.Join(t.TempDir(), "marker")
	got = runProcess(ctx, t.TempDir(), helperArgs(t, "mark", marker), os.Environ(), io.Discard, io.Discard)
	if !got.Canceled {
		t.Fatalf("pre-cancel=%+v", got)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("canceled command started")
	}
}
func TestTimeoutTerminatesOrdinaryDescendants(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("POSIX process groups")
	}
	marker := filepath.Join(t.TempDir(), "marker")
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	var output bytes.Buffer
	started := time.Now()
	got := runProcess(ctx, t.TempDir(), helperArgs(t, "tree", marker), os.Environ(), &output, io.Discard)
	if !got.TimedOut || time.Since(started) > 3*time.Second || !strings.Contains(output.String(), "child started") {
		t.Fatalf("timeout=%+v output=%q", got, output.String())
	}
	time.Sleep(700 * time.Millisecond)
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("descendant survived timeout")
	}
}
func TestLockSerializesAndCancellationDoesNotLeak(t *testing.T) {
	common := t.TempDir()
	release, err := acquireLock(context.Background(), common)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	another, err := acquireLock(ctx, common)
	if another != nil {
		another()
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second owner acquired lock: %v", err)
	}
	release()
	release, err = acquireLock(context.Background(), common)
	if err != nil {
		t.Fatal(err)
	}
	release()
}
func TestLimitedLogIsBoundedAndReportsTruncation(t *testing.T) {
	var output bytes.Buffer
	w := &limitedLog{writer: &output, remaining: 4}
	n, err := w.Write([]byte("abcdef"))
	if err != nil || n != 6 || output.String() != "abcd" || !w.truncated {
		t.Fatalf("n=%d err=%v output=%q truncated=%v", n, err, output.String(), w.truncated)
	}
}
