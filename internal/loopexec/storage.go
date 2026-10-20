package loopexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func within(parent, path string) bool {
	rel, err := filepath.Rel(parent, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Resolve existing ancestors before deciding whether an output is inside source.
func resolveDestination(path string) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	var suffix []string
	parent := path
	for {
		resolved, err := filepath.EvalSymlinks(parent)
		if err == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return resolved, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		next := filepath.Dir(parent)
		if next == parent {
			return "", err
		}
		suffix = append(suffix, filepath.Base(parent))
		parent = next
	}
}

func prepareOutput(ctx context.Context, root, common, output, id string) (string, error) {
	if output == "" {
		output = filepath.Join(root, ".lf-loop", "runs")
	}
	if !filepath.IsAbs(output) {
		output = filepath.Join(root, output)
	}
	output, err := resolveDestination(output)
	if err != nil {
		return "", err
	}
	if within(common, output) {
		return "", errors.New("reports cannot be stored inside Git metadata")
	}
	if within(root, output) {
		rel, err := filepath.Rel(root, output)
		if err != nil {
			return "", err
		}
		probe := exec.CommandContext(ctx, "git", "-C", root, "check-ignore", "-q", "--", rel+string(filepath.Separator))
		if probeErr := probe.Run(); probeErr != nil {
			return "", errors.New("report directory inside the repository must be git-ignored; use .lf-loop/runs or an external path")
		}
		tracked, err := gitOutput(ctx, root, "ls-files", "-z", "--", rel)
		if err != nil {
			return "", err
		}
		if len(tracked) > 0 {
			return "", errors.New("report directory contains tracked source")
		}
	}
	if err := os.MkdirAll(output, 0700); err != nil {
		return "", err
	}
	return os.MkdirTemp(output, id+"-")
}

func saveReport(report Report) error {
	temp, err := os.CreateTemp(report.Directory, ".report-")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer os.Remove(name)
	encoder := json.NewEncoder(temp)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, filepath.Join(report.Directory, "report.json")); err != nil {
		return fmt.Errorf("publish report: %w", err)
	}
	return nil
}
