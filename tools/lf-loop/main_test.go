package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/im-pingo/liveforge/internal/loopexec"
)

func TestCLIRejectsInvalidInputAndDoesNotClaimSuccess(t *testing.T) {
	for _, args := range [][]string{nil, {"unknown"}, {"run"}, {"run", "-task", "/missing/task.json"}, {"run", "-bogus"}} {
		var out, errout bytes.Buffer
		if code := runCLI(context.Background(), args, &out, &errout); code != 2 {
			t.Fatalf("args=%v exit=%d", args, code)
		}
	}
}
func TestCLIProducesUsableJSONAndPropagatesBlockedExit(t *testing.T) {
	root := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.email", "test@example.invalid"}, {"config", "user.name", "Loop Test"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if data, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git: %s %v", data, err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(".lf-loop/\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-qm", "fixture"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if data, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git: %s %v", data, err)
		}
	}
	taskPath := filepath.Join(root, "task.json")
	for _, tc := range []struct {
		command string
		code    int
	}{{"go", 0}, {"./absent", 3}} {
		task := loopexec.Task{SchemaVersion: 1, ID: "cli", Objective: "CLI evidence", Checks: []loopexec.Check{{ID: "probe", Kind: "command", Argv: []string{tc.command, "version"}}}}
		data, _ := json.Marshal(task)
		if err := os.WriteFile(taskPath, data, 0600); err != nil {
			t.Fatal(err)
		}
		var out, errout bytes.Buffer
		code := runCLI(context.Background(), []string{"run", "-root", root, "-task", taskPath, "-json"}, &out, &errout)
		if code != tc.code {
			t.Fatalf("exit=%d stderr=%s", code, errout.String())
		}
		var report loopexec.Report
		if err := json.Unmarshal(out.Bytes(), &report); err != nil {
			t.Fatalf("JSON=%s err=%v", out.String(), err)
		}
		if report.Directory == "" || report.ExitCode() != code {
			t.Fatalf("report=%+v", report)
		}
		if tc.code == 0 {
			for _, timeout := range []string{"0", "-1s", "25h"} {
				out.Reset()
				errout.Reset()
				if code := runCLI(context.Background(), []string{"run", "-root", root, "-task", taskPath, "-timeout", timeout}, &out, &errout); code != 2 {
					t.Fatalf("timeout=%s exit=%d; expected invalid option", timeout, code)
				}
			}
		}
	}
}
