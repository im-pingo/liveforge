# Cluster Schedule URL Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add dynamic target resolution via HTTP callback to the cluster module, so forward targets and origin servers can be determined per-stream at runtime by an external scheduling service.

**Architecture:** A new `Scheduler` component in `module/cluster/scheduler.go` encapsulates HTTP resolution with priority-based fallback to static config. Both `ForwardManager` and `OriginManager` call `scheduler.Resolve()` instead of using their static target lists directly.

**Tech Stack:** Go stdlib (`net/http`, `encoding/json`, `net/http/httptest` for tests)

**Spec:** `docs/superpowers/specs/2026-03-30-cluster-schedule-url-design.md`

---

## File Map

| File | Action | Responsibility |
|------|--------|----------------|
| `config/config.go` | Modify | Add `SchedulePriority` + `ScheduleTimeout` to ForwardConfig and OriginConfig |
| `configs/liveforge.yaml` | Modify | Add `schedule_priority` and `schedule_timeout` entries |
| `module/cluster/scheduler.go` | Create | Scheduler struct, Resolve method, HTTP POST client |
| `module/cluster/scheduler_test.go` | Create | httptest-based tests for all resolution paths |
| `module/cluster/forward.go` | Modify | Replace static `targets` field with `scheduler` field |
| `module/cluster/origin.go` | Modify | Replace static `servers` field with `scheduler` field |
| `module/cluster/module.go` | Modify | Create Schedulers in Init, pass to managers |
| `module/cluster/module_test.go` | Modify | Update constructor calls for new signatures |

---

### Task 1: Config — Add SchedulePriority and ScheduleTimeout fields

**Files:**
- Modify: `config/config.go:264-281`
- Modify: `configs/liveforge.yaml:128-141`

- [ ] **Step 1: Add fields to ForwardConfig**

In `config/config.go`, add two fields to `ForwardConfig`:

```go
type ForwardConfig struct {
	Enabled          bool          `yaml:"enabled"`
	Targets          []string      `yaml:"targets"`
	ScheduleURL      string        `yaml:"schedule_url"`
	SchedulePriority string        `yaml:"schedule_priority"`
	ScheduleTimeout  time.Duration `yaml:"schedule_timeout"`
	RetryMax         int           `yaml:"retry_max"`
	RetryInterval    time.Duration `yaml:"retry_interval"`
}
```

- [ ] **Step 2: Add fields to OriginConfig**

In `config/config.go`, add the same two fields to `OriginConfig`:

```go
type OriginConfig struct {
	Enabled          bool          `yaml:"enabled"`
	Servers          []string      `yaml:"servers"`
	ScheduleURL      string        `yaml:"schedule_url"`
	SchedulePriority string        `yaml:"schedule_priority"`
	ScheduleTimeout  time.Duration `yaml:"schedule_timeout"`
	IdleTimeout      time.Duration `yaml:"idle_timeout"`
	RetryMax         int           `yaml:"retry_max"`
	RetryDelay       time.Duration `yaml:"retry_delay"`
}
```

- [ ] **Step 3: Update YAML config**

In `configs/liveforge.yaml`, add the new fields under both forward and origin:

```yaml
cluster:
  forward:
    enabled: false
    targets: []
    schedule_url: ""
    schedule_priority: schedule_first
    schedule_timeout: 3s
    retry_max: 5
    retry_interval: 3s
  origin:
    enabled: false
    servers: []
    schedule_url: ""
    schedule_priority: schedule_first
    schedule_timeout: 3s
    idle_timeout: 30s
    retry_max: 3
    retry_delay: 2s
```

- [ ] **Step 4: Verify build**

Run: `go build ./...`
Expected: clean build, no errors

- [ ] **Step 5: Commit**

```bash
git add config/config.go configs/liveforge.yaml
git commit -m "feat: add schedule_priority and schedule_timeout to cluster config"
```

---

### Task 2: Scheduler — Core component with tests (TDD)

**Files:**
- Create: `module/cluster/scheduler.go`
- Create: `module/cluster/scheduler_test.go`

- [ ] **Step 1: Write failing tests for Scheduler**

Create `module/cluster/scheduler_test.go`:

```go
package cluster

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSchedulerStaticOnly(t *testing.T) {
	// No URL, only static fallback
	s := NewScheduler("", []string{"rtmp://static/live"}, "schedule_first", 3*time.Second)
	targets, err := s.Resolve("forward", "live/test")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(targets) != 1 || targets[0] != "rtmp://static/live" {
		t.Errorf("targets = %v, want [rtmp://static/live]", targets)
	}
}

func TestSchedulerDynamicOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req scheduleRequest
		json.NewDecoder(r.Body).Decode(&req)
		if req.Action != "forward" || req.StreamKey != "live/test" {
			t.Errorf("unexpected request: %+v", req)
		}
		json.NewEncoder(w).Encode(scheduleResponse{
			Targets: []string{"rtmp://dynamic1/live/test", "rtmp://dynamic2/live/test"},
		})
	}))
	defer srv.Close()

	s := NewScheduler(srv.URL, nil, "schedule_first", 3*time.Second)
	targets, err := s.Resolve("forward", "live/test")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(targets) != 2 {
		t.Errorf("targets count = %d, want 2", len(targets))
	}
}

func TestSchedulerScheduleFirstFallback(t *testing.T) {
	// Dynamic returns error → fallback to static
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	s := NewScheduler(srv.URL, []string{"rtmp://fallback/live"}, "schedule_first", 3*time.Second)
	targets, err := s.Resolve("origin", "live/test")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(targets) != 1 || targets[0] != "rtmp://fallback/live" {
		t.Errorf("targets = %v, want [rtmp://fallback/live]", targets)
	}
}

func TestSchedulerStaticFirstFallback(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		json.NewEncoder(w).Encode(scheduleResponse{
			Targets: []string{"rtmp://dynamic/live/test"},
		})
	}))
	defer srv.Close()

	// Static has entries → should use static, never call HTTP
	s := NewScheduler(srv.URL, []string{"rtmp://static/live"}, "static_first", 3*time.Second)
	targets, err := s.Resolve("forward", "live/test")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(targets) != 1 || targets[0] != "rtmp://static/live" {
		t.Errorf("targets = %v, want [rtmp://static/live]", targets)
	}
	if called {
		t.Error("HTTP should not be called when static_first has entries")
	}
}

func TestSchedulerStaticFirstEmptyFallsThrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(scheduleResponse{
			Targets: []string{"rtmp://dynamic/live/test"},
		})
	}))
	defer srv.Close()

	// Static is empty → should fall through to HTTP
	s := NewScheduler(srv.URL, nil, "static_first", 3*time.Second)
	targets, err := s.Resolve("forward", "live/test")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(targets) != 1 || targets[0] != "rtmp://dynamic/live/test" {
		t.Errorf("targets = %v, want [rtmp://dynamic/live/test]", targets)
	}
}

func TestSchedulerTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond) // longer than timeout
	}))
	defer srv.Close()

	s := NewScheduler(srv.URL, []string{"rtmp://fallback/live"}, "schedule_first", 100*time.Millisecond)
	targets, err := s.Resolve("forward", "live/test")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(targets) != 1 || targets[0] != "rtmp://fallback/live" {
		t.Errorf("targets = %v, want [rtmp://fallback/live]", targets)
	}
}

func TestSchedulerEmptyResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(scheduleResponse{Targets: []string{}})
	}))
	defer srv.Close()

	// Empty targets + no fallback → error
	s := NewScheduler(srv.URL, nil, "schedule_first", 3*time.Second)
	_, err := s.Resolve("forward", "live/test")
	if err == nil {
		t.Error("expected error for empty targets with no fallback")
	}
}

func TestSchedulerBothEmpty(t *testing.T) {
	s := NewScheduler("", nil, "schedule_first", 3*time.Second)
	_, err := s.Resolve("forward", "live/test")
	if err == nil {
		t.Error("expected error when both url and fallback are empty")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./module/cluster/ -run TestScheduler -v`
Expected: compilation error — `NewScheduler`, `scheduleRequest`, `scheduleResponse` not defined

- [ ] **Step 3: Implement Scheduler**

Create `module/cluster/scheduler.go`:

```go
package cluster

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// scheduleRequest is the JSON body sent to the schedule_url endpoint.
type scheduleRequest struct {
	Action    string `json:"action"`
	StreamKey string `json:"stream_key"`
}

// scheduleResponse is the JSON body returned by the schedule_url endpoint.
type scheduleResponse struct {
	Targets []string `json:"targets"`
}

// Scheduler resolves target lists for cluster forwarding and origin pull.
// It supports dynamic resolution via HTTP callback, static fallback lists,
// and priority-based selection between the two.
type Scheduler struct {
	url      string
	fallback []string
	priority string
	client   *http.Client
}

// NewScheduler creates a scheduler.
// If url is empty, only static fallback is used.
// If fallback is empty/nil, only schedule_url is used.
// priority is "schedule_first" (default) or "static_first".
func NewScheduler(url string, fallback []string, priority string, timeout time.Duration) *Scheduler {
	if priority == "" {
		priority = "schedule_first"
	}
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	return &Scheduler{
		url:      url,
		fallback: fallback,
		priority: priority,
		client:   &http.Client{Timeout: timeout},
	}
}

// Resolve returns the target list for a given stream key.
// action is "forward" or "origin".
func (s *Scheduler) Resolve(action, streamKey string) ([]string, error) {
	// No URL configured → static only
	if s.url == "" {
		if len(s.fallback) > 0 {
			return s.fallback, nil
		}
		return nil, errors.New("no schedule_url and no static targets configured")
	}

	// No static fallback → dynamic only
	if len(s.fallback) == 0 {
		targets, err := s.httpResolve(action, streamKey)
		if err != nil {
			return nil, fmt.Errorf("schedule_url failed: %w", err)
		}
		if len(targets) == 0 {
			return nil, errors.New("schedule_url returned empty targets")
		}
		return targets, nil
	}

	// Both configured → use priority
	if s.priority == "static_first" {
		return s.fallback, nil
	}

	// schedule_first (default)
	targets, err := s.httpResolve(action, streamKey)
	if err != nil || len(targets) == 0 {
		if err != nil {
			slog.Warn("schedule_url failed, using static fallback",
				"module", "cluster", "url", s.url, "error", err)
		}
		return s.fallback, nil
	}
	return targets, nil
}

// httpResolve sends a POST request to the schedule_url and parses the response.
func (s *Scheduler) httpResolve(action, streamKey string) ([]string, error) {
	body, err := json.Marshal(scheduleRequest{
		Action:    action,
		StreamKey: streamKey,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	resp, err := s.client.Post(s.url, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("HTTP request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP status %d", resp.StatusCode)
	}

	var result scheduleResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	return result.Targets, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./module/cluster/ -run TestScheduler -v`
Expected: all 8 tests PASS

- [ ] **Step 5: Run full test suite**

Run: `go test ./module/cluster/ -race`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add module/cluster/scheduler.go module/cluster/scheduler_test.go
git commit -m "feat: add Scheduler for dynamic cluster target resolution"
```

---

### Task 3: Integrate Scheduler into ForwardManager

**Files:**
- Modify: `module/cluster/forward.go:180-210,230-265`
- Modify: `module/cluster/module.go:29-38`
- Modify: `module/cluster/module_test.go`

- [ ] **Step 1: Replace static `targets` with `scheduler` in ForwardManager**

In `forward.go`, change the `ForwardManager` struct and constructor:

```go
type ForwardManager struct {
	hub       *core.StreamHub
	eventBus  *core.EventBus
	scheduler *Scheduler
	retryMax  int
	retryDel  time.Duration

	mu     sync.Mutex
	active map[string][]*ForwardTarget
	closed chan struct{}
}

func NewForwardManager(hub *core.StreamHub, bus *core.EventBus, scheduler *Scheduler, retryMax int, retryDelay time.Duration) *ForwardManager {
	if retryMax <= 0 {
		retryMax = 3
	}
	if retryDelay <= 0 {
		retryDelay = 5 * time.Second
	}
	return &ForwardManager{
		hub:       hub,
		eventBus:  bus,
		scheduler: scheduler,
		retryMax:  retryMax,
		retryDel:  retryDelay,
		active:    make(map[string][]*ForwardTarget),
		closed:    make(chan struct{}),
	}
}
```

- [ ] **Step 2: Update onPublish to use scheduler.Resolve**

In `forward.go`, replace the `onPublish` target iteration:

```go
func (fm *ForwardManager) onPublish(ctx *core.EventContext) error {
	stream, ok := fm.hub.Find(ctx.StreamKey)
	if !ok {
		return nil
	}

	fm.mu.Lock()
	defer fm.mu.Unlock()

	if _, exists := fm.active[ctx.StreamKey]; exists {
		return nil
	}

	targets, err := fm.scheduler.Resolve("forward", ctx.StreamKey)
	if err != nil {
		slog.Warn("forward schedule resolve failed", "module", "cluster",
			"stream", ctx.StreamKey, "error", err)
		return nil
	}

	var fts []*ForwardTarget
	for _, target := range targets {
		url := target
		if !containsStreamPath(target) {
			url = fmt.Sprintf("%s/%s", target, ctx.StreamKey)
		}
		ft := NewForwardTarget(ctx.StreamKey, url, stream, fm.retryMax, fm.retryDel)
		fts = append(fts, ft)
		go ft.Run()
	}

	fm.active[ctx.StreamKey] = fts

	fm.eventBus.Emit(core.EventForwardStart, &core.EventContext{
		StreamKey: ctx.StreamKey,
		Extra:     map[string]any{"target_count": len(fts)},
	})

	return nil
}
```

- [ ] **Step 3: Update Module.Init for ForwardManager**

In `module.go`, change the forward manager creation to build a Scheduler:

```go
if cfg.Forward.Enabled && (len(cfg.Forward.Targets) > 0 || cfg.Forward.ScheduleURL != "") {
	fwdScheduler := NewScheduler(
		cfg.Forward.ScheduleURL,
		cfg.Forward.Targets,
		cfg.Forward.SchedulePriority,
		cfg.Forward.ScheduleTimeout,
	)
	m.forward = NewForwardManager(
		hub, bus,
		fwdScheduler,
		cfg.Forward.RetryMax,
		cfg.Forward.RetryInterval,
	)
	slog.Info("cluster forward enabled", "module", "cluster",
		"static_targets", len(cfg.Forward.Targets),
		"schedule_url", cfg.Forward.ScheduleURL)
}
```

- [ ] **Step 4: Update module_test.go ForwardManager constructor calls**

All `NewForwardManager(hub, bus, []string{...}, ...)` calls need a Scheduler instead of a string slice. Replace each with:

```go
// Example: where you had NewForwardManager(hub, bus, []string{"rtmp://target/live/stream"}, 0, 0)
// Replace with:
NewForwardManager(hub, bus, NewScheduler("", []string{"rtmp://target/live/stream"}, "", 0), 0, 0)
```

Apply this pattern to all `NewForwardManager` calls in `module_test.go`.

- [ ] **Step 5: Verify build and tests**

Run: `go test ./module/cluster/ -race -v`
Expected: all tests PASS

- [ ] **Step 6: Commit**

```bash
git add module/cluster/forward.go module/cluster/module.go module/cluster/module_test.go
git commit -m "feat: integrate Scheduler into ForwardManager"
```

---

### Task 4: Integrate Scheduler into OriginManager

**Files:**
- Modify: `module/cluster/origin.go:230-265,279-298`
- Modify: `module/cluster/module.go:40-50`
- Modify: `module/cluster/module_test.go`

- [ ] **Step 1: Replace static `servers` with `scheduler` in OriginManager**

In `origin.go`, change the struct and constructor:

```go
type OriginManager struct {
	hub         *core.StreamHub
	eventBus    *core.EventBus
	scheduler   *Scheduler
	retryMax    int
	retryDelay  time.Duration
	idleTimeout time.Duration

	mu     sync.Mutex
	active map[string]*OriginPull
	closed chan struct{}
}

func NewOriginManager(hub *core.StreamHub, bus *core.EventBus, scheduler *Scheduler, retryMax int, retryDelay, idleTimeout time.Duration) *OriginManager {
	if retryMax <= 0 {
		retryMax = 3
	}
	if retryDelay <= 0 {
		retryDelay = 2 * time.Second
	}
	if idleTimeout <= 0 {
		idleTimeout = 30 * time.Second
	}
	return &OriginManager{
		hub:         hub,
		eventBus:    bus,
		scheduler:   scheduler,
		retryMax:    retryMax,
		retryDelay:  retryDelay,
		idleTimeout: idleTimeout,
		active:      make(map[string]*OriginPull),
		closed:      make(chan struct{}),
	}
}
```

- [ ] **Step 2: Update onSubscribe to use scheduler.Resolve**

In `origin.go`, replace the static `om.servers` usage:

```go
func (om *OriginManager) onSubscribe(ctx *core.EventContext) error {
	stream, ok := om.hub.Find(ctx.StreamKey)
	if !ok {
		return nil
	}

	if stream.Publisher() != nil {
		return nil
	}

	om.mu.Lock()
	defer om.mu.Unlock()

	if _, exists := om.active[ctx.StreamKey]; exists {
		return nil
	}

	servers, err := om.scheduler.Resolve("origin", ctx.StreamKey)
	if err != nil {
		slog.Warn("origin schedule resolve failed", "module", "cluster",
			"stream", ctx.StreamKey, "error", err)
		return nil
	}

	op := NewOriginPull(ctx.StreamKey, servers, stream, om.retryMax, om.retryDelay, om.idleTimeout)
	om.active[ctx.StreamKey] = op

	om.eventBus.Emit(core.EventOriginPullStart, &core.EventContext{
		StreamKey: ctx.StreamKey,
	})

	go func() {
		op.Run()
		om.mu.Lock()
		delete(om.active, ctx.StreamKey)
		om.mu.Unlock()
		om.eventBus.Emit(core.EventOriginPullStop, &core.EventContext{
			StreamKey: ctx.StreamKey,
		})
	}()

	return nil
}
```

- [ ] **Step 3: Update Module.Init for OriginManager**

In `module.go`, change the origin manager creation:

```go
if cfg.Origin.Enabled && (len(cfg.Origin.Servers) > 0 || cfg.Origin.ScheduleURL != "") {
	origScheduler := NewScheduler(
		cfg.Origin.ScheduleURL,
		cfg.Origin.Servers,
		cfg.Origin.SchedulePriority,
		cfg.Origin.ScheduleTimeout,
	)
	m.origin = NewOriginManager(
		hub, bus,
		origScheduler,
		cfg.Origin.RetryMax,
		cfg.Origin.RetryDelay,
		cfg.Origin.IdleTimeout,
	)
	slog.Info("cluster origin pull enabled", "module", "cluster",
		"static_servers", len(cfg.Origin.Servers),
		"schedule_url", cfg.Origin.ScheduleURL)
}
```

- [ ] **Step 4: Update module_test.go OriginManager constructor calls**

All `NewOriginManager(hub, bus, []string{...}, ...)` calls need a Scheduler. Replace each with:

```go
// Example: where you had NewOriginManager(hub, bus, []string{"rtmp://origin/live"}, 0, 0, 0)
// Replace with:
NewOriginManager(hub, bus, NewScheduler("", []string{"rtmp://origin/live"}, "", 0), 0, 0, 0)
```

Apply this pattern to all `NewOriginManager` calls in `module_test.go` and `integration_test.go`.

- [ ] **Step 5: Verify all tests pass**

Run: `go test ./module/cluster/ -race -v`
Expected: all tests PASS

Run: `go test ./... -race`
Expected: all packages PASS

- [ ] **Step 6: Commit**

```bash
git add module/cluster/origin.go module/cluster/module.go module/cluster/module_test.go module/cluster/integration_test.go
git commit -m "feat: integrate Scheduler into OriginManager"
```

---

### Task 5: Update Module.Init enable conditions and update PROGRESS.md

**Files:**
- Modify: `module/cluster/module.go`
- Modify: `cmd/liveforge/main.go`
- Modify: `docs/PROGRESS.md`

- [ ] **Step 1: Update main.go enable condition**

In `cmd/liveforge/main.go`, the cluster registration condition should also check for schedule_url:

```go
if cfg.Cluster.Forward.Enabled || cfg.Cluster.Origin.Enabled {
	s.RegisterModule(cluster.NewModule())
}
```

This is already correct — the `Enabled` flag gates the module. No change needed. Verify it matches.

- [ ] **Step 2: Update PROGRESS.md**

Remove `Cluster schedule_url` from the "Config stubs" table in `docs/PROGRESS.md` since it's now implemented.

- [ ] **Step 3: Full verification**

Run: `go build ./...`
Expected: clean build

Run: `go test ./... -race`
Expected: all packages PASS

- [ ] **Step 4: Commit**

```bash
git add docs/PROGRESS.md
git commit -m "docs: mark cluster schedule_url as implemented"
```
