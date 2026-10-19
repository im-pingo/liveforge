package loopexec

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"time"
)

func Run(ctx context.Context, root string, task Task, opts Options) (Report, error) {
	return run(ctx, root, task, opts)
}

func run(parent context.Context, root string, task Task, opts Options) (report Report, retErr error) {
	report = Report{SchemaVersion: 1, Task: task, Status: "error", StartedAt: time.Now().UTC(), Tools: make(map[string]string)}
	if err := task.Validate(); err != nil {
		return report, err
	}
	if opts.Profile == "" {
		opts.Profile = "portable"
	}
	if opts.Profile != "portable" && opts.Profile != "tagged" {
		return report, errors.New("profile must be portable or tagged")
	}
	if opts.Timeout == 0 {
		opts.Timeout = 30 * time.Minute
	}
	if opts.Timeout < 0 || opts.Timeout > 24*time.Hour {
		return report, errors.New("total timeout must be positive and at most 24h")
	}
	ctx, cancel := context.WithTimeout(parent, opts.Timeout)
	defer cancel()
	defer func() {
		report.FinishedAt = time.Now().UTC()
		if retErr != nil {
			report.Status = "error"
			report.Reason = retErr.Error()
			if ctx.Err() != nil {
				report.Status = contextStatus(ctx.Err())
				retErr = nil
			}
		}
		if report.Directory != "" {
			if err := saveReport(report); err != nil {
				retErr = errors.Join(retErr, err)
				report.Status = "error"
			}
		}
	}()
	report.Profile = opts.Profile
	if ctx.Err() != nil {
		report.Status = contextStatus(ctx.Err())
		return report, nil
	}
	top, err := gitOutput(ctx, root, "rev-parse", "--show-toplevel")
	if err != nil {
		return report, err
	}
	root, err = filepath.EvalSymlinks(strings.TrimSpace(string(top)))
	if err != nil {
		return report, err
	}
	commonData, err := gitOutput(ctx, root, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return report, err
	}
	common, err := filepath.EvalSymlinks(strings.TrimSpace(string(commonData)))
	if err != nil {
		return report, err
	}
	report.Root = root
	report.Directory, err = prepareOutput(ctx, root, common, opts.Output, task.ID)
	if err != nil {
		return report, err
	}
	report.AttemptID = filepath.Base(report.Directory)
	data, err := json.Marshal(task)
	if err != nil {
		return report, err
	}
	digest := sha256.Sum256(data)
	report.TaskSHA256 = hex.EncodeToString(digest[:])
	report.Status = "running"
	for _, check := range task.Checks {
		report.Checks = append(report.Checks, CheckResult{ID: check.ID, Kind: check.Kind, Status: "not_run", ExitCode: -1})
	}
	if err := saveReport(report); err != nil {
		return report, err
	}
	release, err := acquireLock(ctx, common)
	if err != nil {
		if ctx.Err() != nil {
			report.Status = contextStatus(ctx.Err())
			report.Reason = "interrupted while waiting for repository lock"
			return report, nil
		}
		return report, err
	}
	defer release()
	env := environment(opts.Profile)
	inputs := append([]string(nil), task.Inputs...)
	if binary := envValue(env, "LF_BINARY"); binary != "" {
		inputs = append(inputs, binary)
	}
	report.Inputs = inputs
	report.Before, err = CaptureSnapshot(ctx, root, inputs)
	if err != nil {
		if ctx.Err() != nil {
			report.Status = contextStatus(ctx.Err())
			return report, nil
		}
		report.Status = "blocked_env"
		report.Reason = err.Error()
		return report, nil
	}
	report.After = report.Before
	cache := make(map[string]error)
	for i, check := range task.Checks {
		if ctx.Err() != nil {
			report.Status = contextStatus(ctx.Err())
			report.Reason = "total task budget exhausted or canceled"
			return report, nil
		}
		report.Checks[i] = executeCheck(ctx, root, report.Directory, check, opts.Profile, env, report.Tools, cache)
		report.Checks[i].Before = report.After.Digest
		snapshotCtx := ctx
		var cleanupCancel context.CancelFunc
		if ctx.Err() != nil {
			snapshotCtx, cleanupCancel = context.WithTimeout(context.Background(), 5*time.Second)
		}
		after, snapshotErr := CaptureSnapshot(snapshotCtx, root, inputs)
		if cleanupCancel != nil {
			cleanupCancel()
		}
		if snapshotErr != nil {
			report.Status = "stale"
			report.Reason = "cannot verify final source: " + snapshotErr.Error()
			return report, nil
		}
		report.After = after
		report.Checks[i].After = after.Digest
		if after.Digest != report.Before.Digest {
			report.Status = "stale"
			report.Reason = "source, index, HEAD or explicit inputs changed during verification"
			return report, nil
		}
		if err := saveReport(report); err != nil {
			return report, err
		}
		if report.Checks[i].Status == "timed_out" || report.Checks[i].Status == "canceled" {
			report.Status = report.Checks[i].Status
			report.Reason = report.Checks[i].Reason
			return report, nil
		}
	}
	report.Status = "passed"
	for _, check := range report.Checks {
		switch check.Status {
		case "passed":
		case "error":
			report.Status = "error"
		case "blocked_env":
			if report.Status == "passed" {
				report.Status = "blocked_env"
			}
		default:
			if report.Status != "error" {
				report.Status = "failed"
			}
		}
	}
	return report, nil
}

func contextStatus(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timed_out"
	}
	return "canceled"
}
