# Independent Runtime Configuration Loader Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a selectable, background-polled configuration loader whose published snapshots can be read without I/O or blocking, then wire it into LiveForge startup and reload handling.

**Architecture:** Keep `config.Config` as the typed application model and add `config/runtime` as an independent manager/source package. Sources return complete immutable documents; one manager goroutine performs source I/O, parse/default/normalize/validate/diff, and atomically publishes `ConfigSnapshot` plus typed keys. `core.Server` receives only validated snapshots and continues to classify unsupported changes as restart-required instead of rebuilding listeners.

**Tech Stack:** Go 1.26, `sync/atomic`, `context`, `net/http`, `gopkg.in/yaml.v3`, Consul HTTP API, `github.com/redis/go-redis/v9`, `httptest`, Go race detector.

**Spec:** `docs/superpowers/specs/2026-08-24-config-loader-design.md`

## Global Constraints

- Runtime reads must be one atomic load plus field access; no file/network I/O, channel wait, or refresh-held mutex.
- All source I/O is owned by the manager background goroutine and bounded by context deadlines.
- Invalid refreshes never replace the last valid snapshot; source failures retain the active snapshot and update status.
- Go version is `>=1.26`; preserve the existing no-CGO and `audiocodec` build profiles.
- Simulcast remains deferred and must be documented as unavailable.
- Every source/config/runtime behavior change updates the relevant `agent-manifest.json`, `llms-full.txt`, `llms.txt`, README files, schema, and a recipe.
- Run focused tests, `go test ./...`, tagged build/tests when FFmpeg is available, and `tools/check-agent-docs_test.sh`.

---

### Task 1: Define runtime model, parser, validation, and immutable publication

**Files:**
- Create: `config/runtime/model.go`
- Create: `config/runtime/parser.go`
- Create: `config/runtime/classification.go`
- Create: `config/runtime/manager.go`
- Test: `config/runtime/manager_test.go`
- Test: `config/runtime/parser_test.go`

**Interfaces:**
- Produces `ConfigSource`, `Snapshot`, `Version`, `Options`, `ConfigSnapshot`, `ChangeClass`, `Change`, `Status`, `Manager`, `NewManager`, `(*Manager).Start`, `(*Manager).Refresh`, `(*Manager).Snapshot`, `(*Manager).Status`, and `(*Manager).Close`.
- `Options` contains `Source ConfigSource`, `PollInterval`, `LoadTimeout`, `Initial *config.Config`, `OnChange func(ChangeSet)`, and bounded callback capacity.
- `ConfigSnapshot.Config` is an owned `*config.Config`; callers must not mutate it. `Snapshot` returns the same immutable pointer until a successful publish.

- [ ] **Step 1: Write failing tests for parse/default/normalize and atomic reads**

```go
func TestParseDocumentAppliesDefaultsAndNormalizesContainer(t *testing.T) {
    cfg, err := ParseDocument([]byte("http_stream:\n  llhls:\n    container: mpeg-ts\n"))
    if err != nil { t.Fatal(err) }
    if cfg.HTTP.Listen != ":8080" { t.Fatalf("default listen = %q", cfg.HTTP.Listen) }
    if cfg.HTTP.LLHLS.Container != "ts" { t.Fatalf("container = %q", cfg.HTTP.LLHLS.Container) }
}

func TestSnapshotReadDoesNotWaitForRefresh(t *testing.T) {
    source := &blockingSource{release: make(chan struct{})}
    m, err := NewManager(Options{Source: source, Initial: &config.Config{Server: config.ServerConfig{Name: "initial"}}})
    if err != nil { t.Fatal(err) }
    defer m.Close()
    done := make(chan struct{})
    go func() { _ = m.Refresh(context.Background()); close(done) }()
    select {
    case <-done:
    case <-time.After(100 * time.Millisecond):
        t.Fatal("refresh call blocked runtime control path")
    }
    if got := m.Snapshot().Config.Server.Name; got != "initial" { t.Fatal(got) }
    close(source.release)
}
```

- [ ] **Step 2: Run focused tests and verify they fail for missing runtime package**

Run: `go test ./config/runtime -run 'TestParseDocument|TestSnapshotRead'`

Expected: FAIL because the package and types do not exist yet.

- [ ] **Step 3: Implement model and parser**

Use `config` defaults via a new exported `config.Defaults()` wrapper (leaving `defaults()` as the internal implementation), unmarshal YAML into the default struct, call `normalize`, deep-copy slices before publication, and validate positive intervals, port ranges, bitrate ordering, TLS pairs, and supported enum values. Compute SHA-256 over normalized marshalled YAML for de-duplication.

- [ ] **Step 4: Implement immutable manager state and startup handshake**

Store the active `*ConfigSnapshot` in `atomic.Pointer[ConfigSnapshot]`. `Start` launches the manager worker, requests one source load with a deadline, and waits for that initial result before returning; the wait is startup-only. The worker then polls and handles a coalescing refresh request channel. `Refresh` only enqueues a request and returns immediately, even while source I/O is blocked. Status counters use atomics or a short-lived status mutex that is never touched by `Snapshot()`.

- [ ] **Step 5: Add change diff and restart classification**

Compare normalized configs by field path. Mark listener/module/TLS/codec/port changes restart-required, server identity immutable, Simulcast deferred/restart-required, and the remaining documented policy fields hot. Publish the full new snapshot while preserving a `PendingRestart` path list.

- [ ] **Step 6: Run focused tests and benchmark read paths**

Run: `go test ./config/runtime -race` and `go test ./config/runtime -bench 'Benchmark(Snapshot|KeyLoad)' -benchmem`.

Expected: PASS; benchmarks must show no blocking operations and no source I/O from read methods.

- [ ] **Step 7: Commit the runtime core**

```bash
git add config/config.go config/runtime
git commit -m "feat: add atomic runtime config manager"
```

### Task 2: Add selectable file, HTTP, Consul, and Redis sources

**Files:**
- Create: `config/runtime/source.go`
- Create: `config/runtime/source_file.go`
- Create: `config/runtime/source_http.go`
- Create: `config/runtime/source_consul.go`
- Create: `config/runtime/source_redis.go`
- Modify: `go.mod`
- Modify: `go.sum`
- Test: `config/runtime/source_test.go`

**Interfaces:**
- Consumes `ConfigSource`, `Snapshot`, and `Version` from Task 1.
- Produces constructors `NewFileSource`, `NewHTTPSource`, `NewConsulSource`, `NewRedisSource` and `SourceConfig` validation.

- [ ] **Step 1: Write source contract tests with fakes and `httptest`**

Cover file modification metadata, HTTP `ETag`/`304`, HTTP size/status limits, Consul KV prefix decoding, Redis hash/prefix conversion, context cancellation, and redacted errors. No test may require a live Consul or Redis server; use `httptest` for Consul and an in-memory RESP test server or a client interface fake for Redis.

- [ ] **Step 2: Run source tests and verify failures**

Run: `go test ./config/runtime -run 'Test(File|HTTP|Consul|Redis)Source'`

Expected: FAIL until source constructors and behavior exist.

- [ ] **Step 3: Implement the common source metadata and file source**

Copy returned bytes before handing them to the manager. File loads use `os.Stat` and `os.ReadFile`, return modification time, and report an explicit error for directories or empty files.

- [ ] **Step 4: Implement HTTP source**

Use an injected `http.Client`, bounded response body (`io.LimitReader`), configured timeout, optional bearer token, and conditional headers from the previous snapshot. Accept `200` and `304`; reject other status codes without including credentials or response bodies in errors.

- [ ] **Step 5: Implement Consul source**

Call `/v1/kv/<prefix>?recurse=true`, send `X-Consul-Token` when configured, decode each KV `Value` from base64, use `X-Consul-Index`/highest `ModifyIndex` as version, and deterministically marshal the key map to YAML bytes.

- [ ] **Step 6: Implement Redis source and add the client dependency**

Use `github.com/redis/go-redis/v9`. Support a hash mode (`HGetAll`) and prefix mode (`SCAN` plus one pipelined `GET` batch), optional version key, TLS, credentials, database, and pool limits. Convert returned keys into a deterministic nested document and close the client in `Close`.

- [ ] **Step 7: Run source tests and race tests**

Run: `go test ./config/runtime -race`.

Expected: PASS with no leaked response bodies, goroutines, or clients.

- [ ] **Step 8: Commit source adapters**

```bash
git add config/runtime go.mod go.sum
git commit -m "feat: support file http consul and redis config sources"
```

### Task 3: Add typed keys and source configuration parsing

**Files:**
- Create: `config/runtime/key.go`
- Create: `config/runtime/source_config.go`
- Modify: `config/config.go`
- Modify: `config/loader.go`
- Test: `config/runtime/key_test.go`
- Test: `config/runtime/source_config_test.go`

**Interfaces:**
- Consumes `Manager` publication from Task 1 and source constructors from Task 2.
- Produces `config.RuntimeConfig`, `BuildSource`, `RegisterKey[T]`, and `Key[T].Load` for local consumers.

- [ ] **Step 1: Write failing tests for registration and source selection**

```go
func TestKeyLoadChangesOnlyAtSnapshotCommit(t *testing.T) {
    // Register an int extractor, publish two snapshots, and assert the key changes with the snapshot.
}
func TestBuildSourceSelectsHTTPAndFallsBackToFile(t *testing.T) {
    // Assert source kind selection and source-specific validation.
}
```

- [ ] **Step 2: Run focused tests and verify failures**

Run: `go test ./config/runtime -run 'Test(Key|BuildSource)'`

Expected: FAIL until typed key and runtime source configuration exist.

- [ ] **Step 3: Add runtime bootstrap configuration fields**

Add `Runtime` to `config.Config` with `Source`, `PollInterval`, `LoadTimeout`, `Path`, `URL`, `Consul`, and `Redis` fields. `config.Load` parses these fields while preserving all existing defaults and environment expansion.

- [ ] **Step 4: Implement source selection and typed keys**

Validate exactly one source kind, require source-specific addresses, and construct the corresponding adapter. Typed keys register a local extractor and store values in an `atomic.Pointer[T]`; duplicate names fail before startup. Key values are updated only inside the manager's publish phase.

- [ ] **Step 5: Run tests and docs contract**

Run: `go test ./config/runtime -race` and `tools/check-agent-docs_test.sh`.

Expected: PASS.

- [ ] **Step 6: Commit source selection and keys**

```bash
git add config/config.go config/loader.go config/runtime
git commit -m "feat: configure runtime source and typed config keys"
```

### Task 4: Wire the manager into `core.Server` and process lifecycle

**Files:**
- Modify: `core/server.go`
- Modify: `cmd/liveforge/main.go`
- Modify: `core/server_test.go`
- Create: `cmd/liveforge/config_runtime_test.go`

**Interfaces:**
- Consumes `runtime.Manager` and `ConfigSnapshot` from Tasks 1-3.
- Produces `Server.ConfigSnapshot`, `Server.UpdateConfigSnapshot`, asynchronous SIGHUP refresh, and clean manager shutdown.

- [ ] **Step 1: Write failing integration tests**

Test that startup rejects a source with no valid first snapshot, SIGHUP only schedules refresh, a source error retains the prior config, and `Server.Config()` remains an atomic read after publication. Test shutdown closes the manager exactly once.

- [ ] **Step 2: Run focused integration tests and verify failures**

Run: `go test ./core ./cmd/liveforge -run 'Test.*Config|Test.*Reload'`

Expected: FAIL until lifecycle wiring is present.

- [ ] **Step 3: Add manager ownership to `Server`**

Store an optional manager pointer, expose it for status/API integration, and have `UpdateConfigSnapshot` apply only hot fields to existing reloadable modules while recording restart-required paths. Keep `UpdateConfig(*config.Config)` for existing tests and callers.

- [ ] **Step 4: Replace synchronous signal reload in `main`**

Load the bootstrap file, construct the manager/source, call `Start` before module registration, register modules from the active snapshot, and call `manager.Refresh` from the SIGHUP branch without file/network work in the signal loop. Close manager after server shutdown.

- [ ] **Step 5: Run full quick verification**

Run: `go test ./...`, `go build ./cmd/liveforge`, and `tools/check-agent-docs_test.sh`.

Expected: PASS.

- [ ] **Step 6: Commit lifecycle integration**

```bash
git add core/server.go core/server_test.go cmd/liveforge
git commit -m "feat: wire background config refresh into server lifecycle"
```

### Task 5: Synchronize schema, recipes, and AI-facing documentation

**Files:**
- Modify: `docs/config/config.schema.json`
- Create: `docs/recipes/runtime-config-sources.md`
- Modify: `agent-manifest.json`
- Modify: `llms-full.txt`
- Modify: `llms.txt`
- Modify: `README.md`
- Modify: `README.zh-CN.md`
- Modify: `docs/PROGRESS.md`

**Interfaces:**
- Consumes the final source/config behavior from Tasks 1-4.
- Produces documented setup and truthful capability/status statements.

- [ ] **Step 1: Extend JSON schema**

Document `runtime.source`, polling and timeout durations, file/HTTP/Consul/Redis fields, credential environment-variable forms, and restart-required semantics. Keep `stream.simulcast` explicitly marked deferred.

- [ ] **Step 2: Add runnable source selection recipe**

Create examples for local file, HTTP with ETag, Consul prefix, and Redis hash/prefix. State that the bootstrap file is read once, later reads are in-memory, remote errors retain the last valid snapshot, secrets must come from environment/secret stores, and listener/module changes still require restart.

- [ ] **Step 3: Update AI-facing project facts**

Update manifest capability/status, prerequisites, verification commands, and limitations; add the runtime config workflow to both `llms` files and both READMEs. Correct stale statements that describe already implemented SIP functionality, and keep Simulcast deferred.

- [ ] **Step 4: Run documentation checks and inspect diffs**

Run: `tools/check-agent-docs_test.sh`, `CHECK_AGENT_DOCS_DIFF=1 tools/check-agent-docs.sh`, and `git diff --check`.

Expected: PASS with no undocumented behavior changes or unsupported release claims.

- [ ] **Step 5: Commit documentation**

```bash
git add agent-manifest.json llms.txt llms-full.txt README.md README.zh-CN.md docs/config/config.schema.json docs/recipes/runtime-config-sources.md docs/PROGRESS.md
git commit -m "docs: document runtime configuration sources and reload semantics"
```

### Task 6: Run release-level verification and report remaining gaps

**Files:**
- No source files unless verification exposes a defect.
- Modify: `docs/PROGRESS.md` only if test/toolchain status or remaining gaps changed.

- [ ] **Step 1: Run focused runtime tests**

Run: `go test ./config/runtime -race -cover`.

- [ ] **Step 2: Run the repository quick suite and build**

Run: `go test ./...` and `go build ./cmd/liveforge`.

- [ ] **Step 3: Run the tagged baseline when FFmpeg is available**

Run: `CGO_ENABLED=1 go build -tags audiocodec ./cmd/liveforge` and `CGO_ENABLED=1 go test -tags audiocodec -race ./...`.

If the environment lacks the required FFmpeg libraries or Go coverage tool, record the exact blocker rather than claiming the suite passed.

- [ ] **Step 4: Run documentation validation**

Run: `tools/check-agent-docs_test.sh`.

- [ ] **Step 5: Review status and remaining unfinished features**

Confirm the manager is active in startup, readers are non-blocking, all four sources have tests, restart-required changes are visible, Simulcast is deferred, and unrelated roadmap items are not described as completed.
