# Loop Executor Implementation Plan

> Execution: native implementation in this session, following the user's request to implement the executor. Use superpowers:executing-plans and test-driven-development.

**Goal:** Deliver a usable deterministic verification executor with honest, durable evidence.

**Architecture:** A standard-library Go package under internal/loopexec owns strict task decoding, profile selection, test-event interpretation, source snapshots, cross-process locking, process cancellation and reports. tools/lf-loop is the CLI. Each invocation runs one bounded pass of a trusted local task; it does not edit code, retry failures, deploy, or equate passed checks with human review.

**Tech Stack:** Go 1.26; encoding/json; os/exec; crypto/sha256; POSIX process groups and flock on Linux/macOS. Unsupported hosts fail explicitly.

**Spec:** docs/superpowers/specs/2026-10-15-loop-engineering-design.md, executor stages 1–2. CI rollout remains a separate stage.

## Global Constraints

- Preserve current source/module boundaries and the existing design/navigation changes.
- Work on codex/loop-executor; no main-branch implementation, commit, push or release is needed for this request.
- No new dependencies. Portable checks force CGO_ENABLED=0; tagged checks require CGO and use audiocodec/race.
- Existing Xcode license failure blocks native verification; record it and run independent portable checks.
- Store private, unique run directories outside tracked source or in ignored .lf-loop/.
- Record argv and allowlisted environment only. Task commands are trusted local code and run directly without an implicit shell.
- Maintain AGENTS, manifest, llms, README pair and a recipe for the new supported workflow.

## Review Focus

- Empty, malformed, incomplete or skipped Go test events must never become a passing check.
- Deadline/cancellation must terminate ordinary child process groups and release the lock.
- Changes in staged, unstaged, untracked or explicitly supplied input content must invalidate evidence.
- Two invocations across worktrees must serialize on the common Git directory; cancellation while waiting must remain bounded.
- Missing prerequisites, failed assertions, failed commands and infrastructure errors must retain different results and nonzero CLI exits.

## Task 1: Task protocol and event oracle

Files: internal/loopexec/task.go, events.go, task_test.go, events_test.go.

Interfaces: DecodeTask(io.Reader) (Task, error); Task.Validate() error; event stream accumulates package/test outcomes and checks exact required/allowed test references.

- [x] Write behavioral tests for strict JSON, duplicate IDs, invalid regex/timeouts, incompatible fields, zero tests, failed/skipped/missing required tests, malformed and incomplete events.
- [x] Observe failures with compilable minimal interfaces, then implement validation and event interpretation.
- [x] Run CGO_ENABLED=0 go test ./internal/loopexec -count=1.

Task shape: schema_version=1, id, objective, optional inputs, checks. A check is command (argv) or go-test (packages, run, required_tests, allow_skips), plus timeout and require_browser. Go-test commands are generated with -json -count=1 and the selected profile.

## Task 2: Execution boundaries

Files: internal/loopexec/snapshot.go, process.go, platform_unix.go, platform_other.go, boundary_test.go.

Interfaces: CaptureSnapshot(ctx, root, inputs) (Snapshot, error); runProcess(ctx, root, argv, env, stdout, stderr) processResult; acquireLock(ctx, commonGitDir) (release, error).

- [x] Add real temporary-repository tests for dirty/index/untracked/deleted/symlink/input changes.
- [x] Add real helper-process tests for exit codes, deadline, cancellation, child cleanup and bounded output.
- [x] Add concurrent lock ownership, cancellation and release tests.
- [x] Implement boundaries and rerun focused tests.

## Task 3: Runner and CLI

Files: internal/loopexec/runner.go, report.go, runner_test.go; tools/lf-loop/main.go, main_test.go; tools/lf-loop/tasks/*.json.

Interfaces: Run(ctx, root, task, Options) (Report, error), Report.ExitCode() int. Options selects portable/tagged and output location; caller context bounds the complete invocation.

- [x] Test a real fixture module through Go's JSON output: pass, fail, zero tests, skip, explicit skip allowance, missing prerequisite, source mutation and independent checks after failure.
- [x] Test report persistence, unique attempts, unsafe output paths, pre-canceled runs, timeout and CLI exits.
- [x] Implement CLI flags, signal cancellation, run/check snapshots, immutable report locations, direct argv execution and preflight.
- [x] Supply runnable core/WHEP pilot tasks with exact existing required tests.
- [x] Run pilot on actual LiveForge; verify both passed evidence and blocked tagged profile.

Exit contract: 0 passed supplied checks; 1 failed/invalid evidence/stale; 2 invalid task or runner error; 3 environment blocked; 124 timeout; 130 cancellation. No built-in retries. Overall failure cannot be erased by a later passing check.

## Task 4: Documentation and verification

Files: AGENTS.md, agent-manifest.json, llms.txt, llms-full.txt, README.md, README.zh-CN.md, .gitignore, docs/recipes/loop-executor.md, design status.

- [x] Document JSON format, profiles, required/allowed tests, snapshot scope, platform constraints, lock scope, report privacy and exits.
- [x] Run focused tests, complete portable suite, portable CLI/server builds, documentation checks and diff checks. Results and unresolved full-suite instability are recorded below; this checkbox records execution, not a green full-suite gate.
- [x] Attempt required native baseline only when prerequisites are available; preserve the concrete environment blocker otherwise.
- [x] Obtain fresh code review against the working diff, fix material findings with regression tests, then rerun affected checks.

## Progress

Implemented on `codex/loop-executor`. No media runtime behavior or external dependencies changed. Shared interfaces: Task feeds Runner and event oracle; Snapshot and processResult feed Report.

Independent review reproduced three material defects: tests outside a package lifetime, signaled commands classified as missing prerequisites, and diagnostic text hiding real failures. Regression tests failed before the fixes and pass afterward. The same pass preserved infrastructure exit 2, sorted all unapproved-skip diagnostics, preserved configured Go toolchain selection, rejected nonpositive explicit CLI budgets, and preserved timeout/cancellation status during early repository discovery. The discovery timeout was reproduced before its fix.

Validation observed on 2026-10-15, Go 1.26.0, darwin/arm64:

- Final focused executor and CLI tests passed (7.841 s and 3.403 s respectively); `CGO_ENABLED=0 go vet ./internal/loopexec ./tools/lf-loop` passed. These checks include the final discovery-timeout fix, added after the full suite began.
- Portable executor and server binaries built successfully.
- Actual core task passed: six tests/subtests plus the documentation check. Actual WHEP task passed: both required tests. Source digests matched at every observation boundary.
- Tagged core task returned exit 3 with the Xcode license prerequisite error; the independent documentation check still passed. A direct tagged server build independently failed at `runtime/cgo` for the same reason. Tagged/race and tagged media browser acceptance remain unverified in this environment.
- Full portable coverage run used a freshly built `LF_BINARY`: `CGO_ENABLED=0 LF_BINARY=/absolute/path/to/bin/liveforge-loop-verify go test -json -count=1 -coverprofile=.lf-loop/portable-coverage-normal.out ./...`. It returned exit 1: 54 packages passed, two packages failed; 2,875 tests/subtests passed, two failed, none skipped. `TestClusterOriginEdge` passed in 6.54 s. Failures were `TestRTMPPush` (millisecond duration rounded to zero) and `TestHLSPlay` (no audio/video frames received). These media sources were unchanged by the executor implementation. A bounded isolated recheck of both tests with `-count=3` passed all six executions; the failed full-suite result is retained, and the whole suite is not claimed green.
- The earlier coverage attempt had 55 passing packages and one failing package: an audit permission fixture failed because the invocation applied `umask 077` to both logging and tests. The fixture explicitly creates an unsafe 0644 file, which that mask changed to 0600. An isolated 077/022 comparison reproduced failure/pass. The second run created private log files separately and restored the normal 022 mask. No audit source was changed.
- `tools/check-agent-docs_test.sh`, `CHECK_AGENT_DOCS_DIFF=1 tools/check-agent-docs.sh`, and `git diff --check` passed. Native/race/browser gates remain blocked as described above.

Local run evidence (ignored, never committed): core `core-invariants-3872933258`, WHEP `whep-continuity-3882603884`, tagged blocker `core-invariants-696777441`, all under `.lf-loop/runs/`. The earlier failed pilot `core-invariants-257761405` is retained separately; it exposed the toolchain-selection bug and was not rewritten.

Full-suite logs and coverage remain local under `.lf-loop/portable-suite*` and `.lf-loop/portable-coverage*`; the bounded media recheck is `.lf-loop/media-failure-recheck.jsonl`. Evidence is kept per attempt and is not committed.
