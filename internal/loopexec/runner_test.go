package loopexec

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixtureModule(t *testing.T) string {
	root := fixtureRepo(t)
	writeFixture(t, root, "go.mod", "module example.invalid/loopfixture\n\ngo 1.26\n")
	writeFixture(t, root, "sample_test.go", "package fixture\nimport \"testing\"\nfunc TestPass(t *testing.T){}\nfunc TestFail(t *testing.T){t.Error(\"real assertion failed\")}\nfunc TestSkip(t *testing.T){t.Skip(\"optional fixture\")}\n")
	return root
}
func TestRunnerUsesRealGoEventsAndKeepsEveryAttempt(t *testing.T) {
	root := fixtureModule(t)
	previous := ""
	for _, tc := range []struct {
		pattern, want     string
		required, allowed []TestRef
	}{
		{"^TestPass$", "passed", []TestRef{{"example.invalid/loopfixture", "TestPass"}}, nil},
		{"^TestFail$", "failed", nil, nil},
		{"^Typo$", "invalid_evidence", nil, nil},
		{"^TestSkip$", "invalid_evidence", nil, nil},
		{"TestPass|TestSkip", "passed", nil, []TestRef{{"example.invalid/loopfixture", "TestSkip"}}},
		{"^TestPass$", "invalid_evidence", []TestRef{{"example.invalid/loopfixture", "TestMissing"}}, nil},
	} {
		t.Run(tc.pattern+"-"+tc.want, func(t *testing.T) {
			task := validTask()
			task.Checks = []Check{{ID: "go", Kind: "go-test", Packages: []string{"."}, Run: tc.pattern, RequiredTests: tc.required, AllowSkips: tc.allowed}}
			report, err := Run(context.Background(), root, task, Options{})
			if err != nil || len(report.Checks) != 1 || report.Checks[0].Status != tc.want {
				t.Fatalf("report=%+v error=%v", report, err)
			}
			if (report.ExitCode() == 0) != (tc.want == "passed") {
				t.Fatalf("false exit code: %d", report.ExitCode())
			}
			if previous == report.Directory || report.Directory == "" {
				t.Fatal("attempt overwrote evidence")
			}
			previous = report.Directory
			data, err := os.ReadFile(filepath.Join(report.Directory, "report.json"))
			if err != nil {
				t.Fatal(err)
			}
			var saved Report
			if err := json.Unmarshal(data, &saved); err != nil {
				t.Fatal(err)
			}
			if saved.Status != report.Status || saved.After.Digest == "" {
				t.Fatal("incomplete persisted evidence")
			}
			info, err := os.Stat(filepath.Join(report.Directory, "report.json"))
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0600 {
				t.Fatalf("report permissions=%v", info.Mode())
			}
		})
	}
}
func TestRunnerContinuesIndependentChecksButKeepsFailure(t *testing.T) {
	root := fixtureRepo(t)
	task := validTask()
	task.Checks = []Check{
		{ID: "missing", Kind: "command", Argv: []string{"./missing-command"}},
		{ID: "failed", Kind: "command", Argv: helperArgs(t, "exit", "")},
		{ID: "independent", Kind: "command", Argv: []string{"go", "version"}},
	}
	report, err := Run(context.Background(), root, task, Options{})
	if err != nil || len(report.Checks) != 3 {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	if report.Checks[0].Status != "blocked_env" || report.Checks[1].Status != "failed" || report.Checks[2].Status != "passed" || report.ExitCode() != 1 {
		t.Fatalf("report=%+v", report)
	}
}
func TestRunnerInvalidatesSourceMutationAndStopsRemainingChecks(t *testing.T) {
	root := fixtureRepo(t)
	task := validTask()
	task.Checks = []Check{
		{ID: "mutate", Kind: "command", Argv: helperArgs(t, "mark", filepath.Join(root, "new-input"))},
		{ID: "later", Kind: "command", Argv: helperArgs(t, "mark", filepath.Join(root, "must-not-run"))},
	}
	report, err := Run(context.Background(), root, task, Options{})
	if err != nil || report.Status != "stale" || len(report.Checks) != 2 || report.Checks[1].Status != "not_run" {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	if _, err := os.Stat(filepath.Join(root, "must-not-run")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("ran check after source invalidation")
	}
}
func TestRunnerRejectsSourceOutputAndUnknownProfile(t *testing.T) {
	root := fixtureRepo(t)
	task := validTask()
	for _, opts := range []Options{{Output: filepath.Join(root, "source-output")}, {Profile: "imaginary"}} {
		if _, err := Run(context.Background(), root, task, opts); err == nil {
			t.Fatal("accepted unsafe or invalid options")
		}
	}
}
func TestRunnerRecordsTimeoutAndPreCanceledAttempt(t *testing.T) {
	root := fixtureRepo(t)
	task := validTask()
	task.Checks = []Check{{ID: "slow", Kind: "command", Argv: helperArgs(t, "tree", filepath.Join(t.TempDir(), "child")), Timeout: "200ms"}}
	report, err := Run(context.Background(), root, task, Options{})
	if err != nil || report.Status != "timed_out" || report.ExitCode() != 124 {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	report, err = Run(ctx, root, task, Options{})
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if report.ExitCode() != 130 {
		t.Fatalf("pre-cancel report=%+v err=%v", report, err)
	}
}
func TestRunnerRequiresConfiguredBrowserAndDoesNotStoreUnrelatedSecrets(t *testing.T) {
	t.Setenv("CHROME_BIN", filepath.Join(t.TempDir(), "absent-chrome"))
	t.Setenv("LOOP_TEST_SECRET", "never-record-me")
	root := fixtureRepo(t)
	task := validTask()
	task.Checks = []Check{{ID: "browser", Kind: "command", Argv: []string{"go", "version"}, RequireBrowser: true}}
	report, err := Run(context.Background(), root, task, Options{Timeout: time.Second * 10})
	if err != nil || report.Status != "blocked_env" {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	data, _ := json.Marshal(report)
	if strings.Contains(string(data), "never-record-me") {
		t.Fatal("unrelated environment secret persisted")
	}
}

func TestRunnerKeepsDeadlineStatusDuringRepositoryDiscovery(t *testing.T) {
	root := fixtureRepo(t)
	bin := t.TempDir()
	// A slow Git probe deterministically expires after setup begins.
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nexec sleep 10\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	report, err := Run(context.Background(), root, validTask(), Options{Timeout: 100 * time.Millisecond})
	if err != nil || report.Status != "timed_out" || report.ExitCode() != 124 || report.FinishedAt.IsZero() {
		t.Fatalf("status=%s exit=%d finished=%v err=%v", report.Status, report.ExitCode(), report.FinishedAt, err)
	}
}

func TestRunnerSignalIsFailureAndLogTextCannotHideAssertions(t *testing.T) {
	root := fixtureModule(t)
	writeFixture(t, root, "environment_test.go", "package fixture\nimport \"testing\"\nfunc TestEnvironmentText(t *testing.T){t.Error(\"expected error text: required browser does not advertise H.264; You have not agreed to the Xcode license\")}\n")
	for _, check := range []Check{
		{ID: "signal", Kind: "command", Argv: helperArgs(t, "signal", "")},
		{ID: "text-command", Kind: "command", Argv: helperArgs(t, "fake-env", "")},
		{ID: "text-test", Kind: "go-test", Packages: []string{"."}, Run: "^TestEnvironmentText$"},
	} {
		task := validTask()
		task.Checks = []Check{check}
		report, err := Run(context.Background(), root, task, Options{})
		if err != nil || report.Status != "failed" || report.ExitCode() != 1 {
			t.Fatalf("%s: report=%+v err=%v", check.ID, report, err)
		}
	}
}
func TestEnvironmentPreservesConfiguredGoToolchainSelection(t *testing.T) {
	t.Setenv("GOTOOLCHAIN", "go1.26.0+auto")
	if got := envValue(environment("portable"), "GOTOOLCHAIN"); got != "go1.26.0+auto" {
		t.Fatalf("toolchain selection was overridden: %s", got)
	}
}
func TestRunnerInfrastructureErrorKeepsExitTwo(t *testing.T) {
	root := fixtureRepo(t)
	task := validTask()
	task.Checks = []Check{
		{ID: "prime", Kind: "command", Argv: helperArgs(t, "collide", filepath.Join(root, ".lf-loop", "runs"))},
		{ID: "later", Kind: "command", Argv: []string{"go", "version"}},
		{ID: "failed-after-error", Kind: "command", Argv: helperArgs(t, "exit", "")},
	}
	report, err := Run(context.Background(), root, task, Options{})
	if err != nil || report.Status != "error" || report.ExitCode() != 2 {
		t.Fatalf("report=%+v err=%v", report, err)
	}
}
