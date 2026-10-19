# Loop Engineering executor

`lf-loop` runs a bounded pass of a trusted local verification task and saves evidence. It is available from source on Linux and macOS, needs Go 1.26+ and Git, and has no native dependency of its own. Its selected checks may need FFmpeg, a C compiler or Chrome.

## Quick start

From the repository root:

```sh
CGO_ENABLED=0 go build -o bin/lf-loop ./tools/lf-loop
bin/lf-loop run -task tools/lf-loop/tasks/core.json
bin/lf-loop run -task tools/lf-loop/tasks/whep.json
```

Each invocation creates a unique private directory under `.lf-loop/runs/` with `report.json` and per-check stdout/stderr logs. The path is printed at the end. Use `-json` for the final report on stdout, `-out /absolute/external/directory` for another location, and `-timeout 10m` to bound the whole invocation, including lock wait. The default total budget is 30 minutes; each check defaults to 5 minutes. Positive budgets up to 24 hours are accepted. Cleanup and the final source check may take up to five additional seconds after a total deadline.

Use the built binary when consuming exit codes. A `go run` wrapper does not preserve every child exit code.

## Profiles and existing gates

- `-profile portable` is the default: generated Go test commands use `CGO_ENABLED=0`, `-json -count=1` and a timeout. This profile has no audio transcoding and does not run race.
- `-profile tagged` uses `CGO_ENABLED=1`, `-tags audiocodec -race`, plus JSON events, uncached test execution and a timeout. Go and C compiler prerequisites are checked first. On macOS, the repository's vendored FFmpeg libraries are still required; system pkg-config availability alone does not prove this build works.
- Both profiles clear inherited `GOFLAGS`, set `GOWORK=off`, and select the executor host's GOOS/GOARCH. Configured `GOTOOLCHAIN` selection is preserved, including Go's automatic toolchain selection; the preflight records the selected Go version. A command check receives that environment but its argv is literal: extra build tags or command options must be written explicitly.

Run the real media browser gate with the tagged profile:

```sh
CHROME_BIN='/Applications/Google Chrome.app/Contents/MacOS/Google Chrome' \
LIVEFORGE_PROTOCOL_MATRIX_SOAK=60s \
bin/lf-loop run -profile tagged -task tools/lf-loop/tasks/media-browser.json
```

On Linux CI, the current browser path is `/usr/bin/google-chrome`. A check with `require_browser: true` selects a browser and forces `LIVEFORGE_REQUIRE_BROWSER=1`. An explicit invalid `CHROME_BIN` never falls back. Version startup is preflight; H.264 and decoding capability still require the actual browser test. Forgetting the tagged profile cannot pass this task because its required test is absent from portable builds.

The supplied core and WHEP tasks are focused pilots. They do not replace the full tagged baseline or `tools/check-review-regressions.sh`. Existing scripts can be composed as command checks, for example argv `["bash","tools/check-review-regressions.sh"]` with a sufficient timeout and `require_browser: true`. That script explicitly enables CGO for its final matrix. Its text output is retained, but only a `go-test` check receives the executor's per-test JSON interpretation.

## Task JSON contract

Task files contain one JSON document, at most 1 MiB. Unknown fields are rejected. They are separate from LiveForge server configuration.

```json
{
  "schema_version": 1,
  "id": "source-generation",
  "objective": "An old publisher must not write into a replacement generation.",
  "checks": [
    {
      "id": "generation",
      "kind": "go-test",
      "packages": ["./core"],
      "run": "^TestStreamPublisherGenerationIsolation$",
      "timeout": "2m",
      "required_tests": [
        {
          "package": "github.com/im-pingo/liveforge/core",
          "test": "TestStreamPublisherGenerationIsolation"
        }
      ]
    }
  ]
}
```

Task fields:

- `schema_version`: exactly 1.
- `id`: 1–64 ASCII letters/digits/dots/underscores/hyphens; the first character is alphanumeric.
- `objective`: nonempty human-readable intent; passing checks does not prove arbitrary prose.
- `inputs`: optional existing input files, including fixtures outside tracked source. Relative paths resolve from the Git root. Explicit inputs must exist and be regular files or symlinks to regular files. Up to 256 may be declared.
- `checks`: 1–128 checks with unique IDs, executed in order.

A check has `id`, `kind`, optional positive `timeout`, and optional `require_browser`.

- `kind: "go-test"` requires `packages`; accepts a `run` expression, `required_tests` and `allow_skips`. Each test reference has the **full Go import path** in `package` and the exact test/subtest name in `test`. Required and allowed-skip references must be unique and disjoint. Package arguments cannot inject flags.
- `kind: "command"` requires a nonempty `argv` array and cannot declare Go-test fields. There is no implicit shell expansion. Write an explicit shell invocation only for a trusted script.

Task file paths resolve from the invoking directory; checks always execute at the selected repository's Git root. `-root` selects the repository. Paths inside task `inputs` or command argv should account for that root.

Go-test success requires a successful command, complete valid JSON evidence, at least one passing test and passing package, no failed test/package, every required test passing, and no skip except exact entries in `allow_skips`. A zero-match `-run`, a required test excluded by build tags, malformed/truncated evidence, or an unfinished test/package fails validation. All-skipped execution also fails. Subtests count separately; a Go build tag can exclude tests without emitting skip events.

## Reports and completion semantics

`report.json` records the task and its SHA-256, attempt ID, selected profile, root, start/end times, before/after source digests, explicit inputs, observed tool versions, check argv, executable hash, allowlisted environment, exit status, test outcomes and log paths. No full environment dump is stored. Check logs are bounded to 32 MiB per stream; exceeding the bound cannot produce passing evidence.

The report is atomically replaced after checks and at completion. Each attempt has a new directory: earlier failures are never overwritten by a retry. During a hard crash or SIGKILL, a report may remain `running`; that state is not success. Ctrl-C and SIGTERM are handled as cancellation. Startup deadlines and cancellation preserve their exit codes even before repository discovery completes; no on-disk report exists until a safe output directory has been created. Ordinary descendants in the check's process group are killed on cancellation/timeout, and leftover group processes are cleaned up after the leader finishes. Commands that deliberately daemonize into another session are outside this process-group guarantee.

Exit codes:

- 0: supplied checks passed against the observed source boundaries.
- 1: failed check, invalid test evidence, or stale source evidence.
- 2: invalid task/options or executor infrastructure error.
- 3: blocked environment/prerequisite.
- 124: deadline.
- 130: cancellation.

A command failure does not prevent independent later checks from collecting evidence. A later pass cannot erase an earlier failure. Source invalidation, timeout or cancellation stops further checks. Missing executables and failed prerequisite probes report `blocked_env`. A Go build failure before any tests run can also be classified as blocked when its complete logs contain a recognized missing native prerequisite. Command log text, signaled processes and failed assertions—including browser H.264 assertions—remain failures. This conservative classification never exempts a required gate. Infrastructure errors retain exit 2 even when other checks fail.

Passing supplied checks is the verification step of the engineering loop. Review, acceptance, merge and release remain distinct decisions. The executor does not modify code, retry until green, interpret an AI completion message, or create a scheduled job.

## Snapshot and lock boundaries

Snapshots bind HEAD, index entries (including staged state), tracked final file contents, deletions, untracked nonignored files, file modes and source symlink targets. Explicit input files additionally hash their resolved contents. A configured `LF_BINARY` is automatically added to explicit inputs; build a current binary **before** starting a task that uses it.

Ignored outputs are excluded unless explicitly listed as inputs. The executor cannot infer hidden dependencies of arbitrary scripts, remote services, network state or fixtures that were not declared. Source symlinks record link identity; declare a symlink as an explicit input when the external target's content matters. Submodules/directories used as explicit inputs are rejected.

Snapshots are boundary observations, not an immutable filesystem or a watcher. A source edit restored between observations may escape detection. Use an isolated checkout and avoid editing source during a run; do not claim the digest provides stronger isolation.

Reports must be outside the repository or in an ignored directory with no tracked source. Existing symlink ancestors are resolved before this check. `.lf-loop/` is ignored by the repository. Reports/logs use private permissions, but trusted commands may print credentials or private content: do not commit or publish logs without reviewing them.

All lf-loop invocations sharing a Git common directory hold the same OS lock for their run, including across linked worktrees. This serializes the repository's legacy cluster-port fixtures and releases the lock on process exit. Direct `go test` or unrelated clones do not participate; do not run those cluster test processes concurrently.

## Verify the executor

```sh
CGO_ENABLED=0 go test ./internal/loopexec ./tools/lf-loop -count=1
tools/check-agent-docs_test.sh
CHECK_AGENT_DOCS_DIFF=1 tools/check-agent-docs.sh
```

Tests exercise real temporary Git repositories, real Go test event streams, process exit/deadline/descendant behavior, lock contention, explicit skip handling, source mutation, report persistence and CLI exit propagation.
