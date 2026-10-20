package loopexec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

func executeCheck(ctx context.Context, root, dir string, check Check, profile string, baseEnv []string, tools map[string]string, cache map[string]error) (result CheckResult) {
	result = CheckResult{ID: check.ID, Kind: check.Kind, Status: "error", ExitCode: -1, StartedAt: time.Now().UTC()}
	defer func() { result.FinishedAt = time.Now().UTC() }()
	duration, _ := check.duration()
	checkCtx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()
	env, err := prerequisites(checkCtx, root, check, profile, baseEnv, tools, cache)
	result.Environment = recordedEnvironment(env)
	if err != nil {
		result.Status = "blocked_env"
		result.Reason = err.Error()
		if checkCtx.Err() != nil {
			result.Status = contextStatus(checkCtx.Err())
		}
		return result
	}
	argv := append([]string(nil), check.Argv...)
	if check.Kind == "go-test" {
		argv = []string{"go", "test", "-json", "-count=1", "-timeout=" + duration.String()}
		if profile == "tagged" {
			argv = append(argv, "-tags", "audiocodec", "-race")
		}
		if check.Run != "" {
			argv = append(argv, "-run", check.Run)
		}
		argv = append(argv, check.Packages...)
	}
	result.Argv = argv
	result.Stdout = check.ID + ".stdout.log"
	result.Stderr = check.ID + ".stderr.log"
	stdout, err := os.OpenFile(filepath.Join(dir, result.Stdout), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		result.Reason = err.Error()
		return result
	}
	defer stdout.Close()
	stderr, err := os.OpenFile(filepath.Join(dir, result.Stderr), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		result.Reason = err.Error()
		return result
	}
	defer stderr.Close()
	out := &limitedLog{writer: stdout, remaining: 32 << 20}
	errout := &limitedLog{writer: stderr, remaining: 32 << 20}
	process := runProcess(checkCtx, root, argv, env, out, errout)
	result.ExitCode = process.ExitCode
	result.Executable = process.Executable
	result.ExecutableSHA256 = process.ExecutableSHA256
	result.Truncated = out.truncated || errout.truncated
	if process.TimedOut || process.Canceled {
		result.Status = "canceled"
		if process.TimedOut {
			result.Status = "timed_out"
		}
		result.Reason = "check process group stopped after cancellation or deadline"
		return result
	}
	if !process.Started {
		result.Status = "blocked_env"
		result.Reason = fmt.Sprint(process.Err)
		return result
	}
	var evidenceErr error
	if check.Kind == "go-test" {
		if _, err := stdout.Seek(0, 0); err != nil {
			result.Reason = err.Error()
			return result
		}
		summary, err := parseTestEvents(stdout, check)
		result.Tests = &summary
		evidenceErr = err
	}
	if process.ExitCode != 0 || process.Err != nil || errors.Is(evidenceErr, errTestFailed) {
		result.Status = "failed"
		result.Reason = fmt.Sprintf("command exit %d", process.ExitCode)
		if result.Tests != nil && result.Tests.BuildFailed && len(result.Tests.Outcomes) == 0 &&
			!result.Truncated && errors.Is(evidenceErr, errTestFailed) {
			diagnosticOut, _ := os.ReadFile(filepath.Join(dir, result.Stdout))
			diagnosticErr, _ := os.ReadFile(filepath.Join(dir, result.Stderr))
			if environmentDiagnostic(string(diagnosticOut) + "\n" + string(diagnosticErr)) {
				result.Status = "blocked_env"
				result.Reason = "recognized missing native prerequisite before test execution; see logs"
			}
		}
		return result
	}
	if result.Truncated {
		result.Status = "invalid_evidence"
		result.Reason = "log exceeded 32 MiB per stream"
		return result
	}
	if evidenceErr != nil {
		result.Status = "invalid_evidence"
		result.Reason = evidenceErr.Error()
		return result
	}
	result.Status = "passed"
	return result
}
