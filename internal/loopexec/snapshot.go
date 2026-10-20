package loopexec

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

type Snapshot struct {
	Head   string `json:"head"`
	Digest string `json:"digest"`
	Files  int    `json:"files"`
}

func CaptureSnapshot(ctx context.Context, root string, inputs []string) (Snapshot, error) {
	var result Snapshot
	head, err := gitOutput(ctx, root, "rev-parse", "HEAD")
	if err != nil {
		return result, err
	}
	result.Head = strings.TrimSpace(string(head))
	index, err := gitOutput(ctx, root, "ls-files", "--stage", "-z")
	if err != nil {
		return result, err
	}
	files, err := gitOutput(ctx, root, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if err != nil {
		return result, err
	}
	h := sha256.New()
	hashField(h, result.Head)
	hashField(h, string(index))
	unique := make(map[string]bool)
	for _, name := range strings.Split(string(files), "\x00") {
		if name != "" {
			unique[name] = true
		}
	}
	names := make([]string, 0, len(unique))
	for name := range unique {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		hashField(h, "source:"+name)
		if err := hashFile(ctx, h, filepath.Join(root, name), false); err != nil {
			return result, fmt.Errorf("snapshot %s: %w", name, err)
		}
		result.Files++
	}
	sortedInputs := append([]string(nil), inputs...)
	sort.Strings(sortedInputs)
	for _, input := range sortedInputs {
		path := input
		if !filepath.IsAbs(path) {
			path = filepath.Join(root, path)
		}
		hashField(h, "input:"+input)
		if err := hashFile(ctx, h, path, true); err != nil {
			return result, fmt.Errorf("input %s: %w", input, err)
		}
		result.Files++
	}
	result.Digest = hex.EncodeToString(h.Sum(nil))
	return result, nil
}

func gitOutput(ctx context.Context, root string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...) // #nosec G204 -- Internal callers supply fixed Git operations; paths are trusted local task inputs passed as argv.
	data, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return data, nil
}

func hashField(h hash.Hash, s string) {
	_, _ = fmt.Fprintf(h, "%d:", len(s))
	_, _ = io.WriteString(h, s)
}

func hashFile(ctx context.Context, h hash.Hash, path string, required bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) && !required {
		hashField(h, "missing")
		return nil
	}
	if err != nil {
		return err
	}
	hashField(h, info.Mode().String())
	if info.Mode()&os.ModeSymlink != 0 {
		target, linkErr := os.Readlink(path)
		if linkErr != nil {
			return linkErr
		}
		hashField(h, target)
		if !required {
			return nil
		}
		info, err = os.Stat(path)
		if err != nil {
			return err
		}
	}
	if !info.Mode().IsRegular() {
		return errors.New("snapshot supports regular files and symlinks; submodules/directories need a separate task")
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	content := sha256.New()
	buffer := make([]byte, 64*1024)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := f.Read(buffer)
		if n > 0 {
			_, _ = content.Write(buffer[:n])
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	hashField(h, hex.EncodeToString(content.Sum(nil)))
	return nil
}
