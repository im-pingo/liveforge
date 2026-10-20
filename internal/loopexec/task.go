// Package loopexec runs bounded, deterministic verification tasks.
package loopexec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
)

type Task struct {
	SchemaVersion int      `json:"schema_version"`
	ID            string   `json:"id"`
	Objective     string   `json:"objective"`
	Inputs        []string `json:"inputs,omitempty"`
	Checks        []Check  `json:"checks"`
}

type TestRef struct {
	Package string `json:"package"`
	Test    string `json:"test"`
}

type Check struct {
	ID             string    `json:"id"`
	Kind           string    `json:"kind"`
	Argv           []string  `json:"argv,omitempty"`
	Packages       []string  `json:"packages,omitempty"`
	Run            string    `json:"run,omitempty"`
	RequiredTests  []TestRef `json:"required_tests,omitempty"`
	AllowSkips     []TestRef `json:"allow_skips,omitempty"`
	Timeout        string    `json:"timeout,omitempty"`
	RequireBrowser bool      `json:"require_browser,omitempty"`
}

func DecodeTask(r io.Reader) (Task, error) {
	var task Task
	data, err := io.ReadAll(io.LimitReader(r, (1<<20)+1))
	if err != nil {
		return task, err
	}
	if len(data) > 1<<20 {
		return task, errors.New("task exceeds 1 MiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&task); err != nil {
		return task, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return task, errors.New("task must contain exactly one JSON document")
	}
	return task, task.Validate()
}

var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func (t Task) Validate() error {
	if t.SchemaVersion != 1 {
		return errors.New("schema_version must be 1")
	}
	if !identifier.MatchString(t.ID) {
		return errors.New("invalid task id")
	}
	if strings.TrimSpace(t.Objective) == "" {
		return errors.New("objective is required")
	}
	if len(t.Checks) == 0 || len(t.Checks) > 128 {
		return errors.New("task needs 1..128 checks")
	}
	if len(t.Inputs) > 256 {
		return errors.New("at most 256 explicit input files are allowed")
	}
	for _, input := range t.Inputs {
		if strings.TrimSpace(input) == "" || strings.ContainsRune(input, 0) {
			return errors.New("invalid input path")
		}
	}
	seen := make(map[string]bool)
	for _, check := range t.Checks {
		if !identifier.MatchString(check.ID) || seen[check.ID] {
			return fmt.Errorf("invalid or duplicate check id %q", check.ID)
		}
		seen[check.ID] = true
		if _, err := check.duration(); err != nil {
			return fmt.Errorf("%s: %w", check.ID, err)
		}
		switch check.Kind {
		case "command":
			if len(check.Argv) == 0 || strings.TrimSpace(check.Argv[0]) == "" {
				return fmt.Errorf("%s: command needs argv", check.ID)
			}
			if len(check.Packages) > 0 || check.Run != "" || len(check.RequiredTests) > 0 || len(check.AllowSkips) > 0 {
				return fmt.Errorf("%s: command cannot declare go-test fields", check.ID)
			}
			for _, arg := range check.Argv {
				if strings.ContainsRune(arg, 0) {
					return errors.New("argv contains NUL")
				}
			}
		case "go-test":
			if len(check.Argv) > 0 || len(check.Packages) == 0 {
				return fmt.Errorf("%s: go-test needs packages and cannot declare argv", check.ID)
			}
			for _, pkg := range check.Packages {
				if pkg == "" || strings.HasPrefix(pkg, "-") || strings.ContainsAny(pkg, " \t\r\n\x00") {
					return fmt.Errorf("%s: invalid package %q", check.ID, pkg)
				}
			}
			if _, err := regexp.Compile(check.Run); err != nil {
				return fmt.Errorf("%s: invalid run expression: %w", check.ID, err)
			}
			refs := make(map[TestRef]bool)
			for _, ref := range append(append([]TestRef(nil), check.RequiredTests...), check.AllowSkips...) {
				if ref.Package == "" || ref.Test == "" || refs[ref] {
					return fmt.Errorf("%s: test references must be complete, unique and disjoint", check.ID)
				}
				refs[ref] = true
			}
		default:
			return fmt.Errorf("%s: kind must be command or go-test", check.ID)
		}
	}
	return nil
}

func (c Check) duration() (time.Duration, error) {
	if c.Timeout == "" {
		return 5 * time.Minute, nil
	}
	d, err := time.ParseDuration(c.Timeout)
	if err != nil || d <= 0 || d > 24*time.Hour {
		return 0, errors.New("timeout must be a positive duration up to 24h")
	}
	return d, nil
}
