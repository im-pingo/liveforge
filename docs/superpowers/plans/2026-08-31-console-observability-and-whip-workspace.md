# Console Observability and WHIP Workspace Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give every LiveForge console view one stable visual system, make Streams layout-stable with four simultaneous 60-second trend charts, and replace the WHIP publish modal with a dedicated `/console/publish` workspace.

**Architecture:** Keep one embedded `module/api/console.html` served by both console routes. Add a shared token/component layer in that document, keyed stream-row reconciliation for stable list rendering, and a selected-stream-only one-second detail poller that feeds four low-height inline SVG charts. Keep WHIP signaling and WebRTC stats logic shared with the current implementation.

**Tech Stack:** Embedded HTML/CSS/vanilla JavaScript, Go `net/http` routing, Chromedp browser tests, inline SVG.

**Spec:** `docs/superpowers/specs/2026-08-31-console-observability-and-whip-workspace-design.md`

## Global Constraints

- The module requires Go 1.26 or newer.
- The quick package check is `go test ./...`; tagged FFmpeg verification remains optional for this frontend-only change.
- Run `tools/check-agent-docs_test.sh` after every change.
- Keep `/console` and `/console/publish` behind the existing console session authentication.
- Do not change the REST or WebRTC signaling contracts.
- Keep the demo artifact in `docs/superpowers/demos/` as a visual reference; it is not runtime code.
- Apply the shared visual system to `Streams`, `GB28181`, `Config`, `Cluster`, `SIP Calls`, `Storage`, `Security`, and Publish.

## File Map

- Modify `module/api/routes.go`: register the `/console/publish` GET route.
- Modify `module/api/console.html`: shared Console visual system, stable Streams table, four-chart detail trend panel, route-aware Publish workspace, and shared WHIP lifecycle wiring.
- Modify `module/api/console_management_test.go`: DOM/script and browser probes for stable rows, detail controls, and publish route markup.
- Modify `module/api/console_publish_test.go`: navigate to `/console/publish` and exercise the full-page publish flow.
- Modify `module/api/module_test.go`: verify both console paths return the embedded document.
- Modify `README.md`, `README.zh-CN.md`, `llms.txt`, `llms-full.txt`: document the new console workflow and selected-stream trends.

### Task 1: Register the publish route and route coverage

**Files:**
- Modify: `module/api/routes.go`
- Test: `module/api/module_test.go`

**Interfaces:**
- Produces `GET /console/publish` serving the same embedded console document as `GET /console`.

- [ ] **Step 1: Write the failing route test**

Add a table-driven assertion beside the existing `/console` test:

```go
for _, path := range []string{"/console", "/console/publish"} {
    req := httptest.NewRequest(http.MethodGet, path, nil)
    rec := httptest.NewRecorder()
    mux.ServeHTTP(rec, req)
    if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "LiveForge Console") {
        t.Fatalf("GET %s = %d, want embedded console", path, rec.Code)
    }
}
```

- [ ] **Step 2: Run the focused test to verify it fails**

Run: `go test ./module/api -run '^Test.*Console.*Route|^Test.*Console$' -count=1`

Expected: `/console/publish` is not matched by the mux.

- [ ] **Step 3: Register the route**

Add `mux.HandleFunc("GET /console/publish", h.handleConsole)` next to the existing `/console` registration.

- [ ] **Step 4: Run the focused test to verify it passes**

Run: `go test ./module/api -run '^Test.*Console.*Route|^Test.*Console$' -count=1`

Expected: PASS for both paths.

- [ ] **Step 5: Commit**

```bash
git add module/api/routes.go module/api/module_test.go
git commit -m "feat: add console publish route"
```

### Task 2: Make the Streams list stable and selectable

**Files:**
- Modify: `module/api/console.html` (Streams markup/CSS and `renderStreams`)
- Test: `module/api/console_management_test.go`

**Interfaces:**
- `renderStreams(streams)` keeps rows keyed by `data-stream-key` and updates fixed value nodes in place.
- A selected row is represented by `selectedStreamKey` and calls `selectStream(key)`.

- [ ] **Step 1: Add failing DOM/script assertions**

Extend `TestConsoleProtocolLabMediaAndCacheRendering` to assert a `<colgroup>`/fixed layout, `data-stream-key`, and stable node identity:

```js
renderStreams(firstPayload);
var firstRow = document.querySelector('#tbody tr[data-stream-key="sip/lab"]');
renderStreams(secondPayload);
return {
  sameRow: firstRow === document.querySelector('#tbody tr[data-stream-key="sip/lab"]'),
  selectedKey: document.querySelector('#tbody tr[data-stream-key="sip/lab"]').dataset.streamKey,
  cacheText: document.querySelector('#tbody tr[data-stream-key="sip/lab"] .gop-value-duration').textContent
};
```

Assert `sameRow` is true and the duration slot changes without replacing the row.

- [ ] **Step 2: Run the focused browser test to verify it fails**

Run: `go test ./module/api -run '^TestConsoleProtocolLabMediaAndCacheRendering$' -count=1`

Expected: current renderer replaces `tbody` children and has no keyed row or fixed GOP slots.

- [ ] **Step 3: Add fixed table tracks and stable GOP markup**

Add a `<colgroup>` with explicit widths, set `table { table-layout: fixed; }`, and render GOP values as stable elements with classes such as `.gop-value-frames`, `.gop-value-duration`, `.gop-value-generation`, and `.gop-value-media`. Keep values tabular and truncate stream/publisher text with ellipsis.

- [ ] **Step 4: Implement keyed row reconciliation and row selection**

Maintain a `streamRows` map keyed by stream key. Create a row only when absent, update its existing value nodes on refresh, remove keys no longer returned, and reorder rows with a document fragment in sorted order. Add `tabindex="0"`, `role="button"`, Enter/Space handlers, and a selected class. Keep current action-button behavior unchanged.

- [ ] **Step 5: Run the focused test to verify it passes**

Run: `go test ./module/api -run '^TestConsoleProtocolLabMediaAndCacheRendering$' -count=1`

Expected: PASS with stable row identity and fixed GOP slot text.

- [ ] **Step 6: Commit**

```bash
git add module/api/console.html module/api/console_management_test.go
git commit -m "feat: stabilize streams table rendering"
```

### Task 3: Add selected-stream trend sampling and four simultaneous charts

**Files:**
- Modify: `module/api/console.html` (detail markup/CSS and stream trend JavaScript)
- Test: `module/api/console_management_test.go`

**Interfaces:**
- `selectStream(streamKey)` opens the detail panel and starts a selected-stream poll.
- `stopStreamTrendPolling()` cancels the one-second timer and aborts its request.
- `renderStreamTrend()` draws four metrics from at most 60 samples.

- [ ] **Step 1: Add failing trend-panel assertions**

Add a browser probe that calls `selectStream("sip/lab")`, stubs `apiFetch` for the detail endpoint, invokes the sample callback twice, and asserts the panel is open, the sample count is `2 / 60`, and the SVG path is non-empty.

- [ ] **Step 2: Run the focused test to verify it fails**

Run: `go test ./module/api -run '^TestConsole.*Trend|^TestConsoleProtocolLabMediaAndCacheRendering$' -count=1`

Expected: the detail elements and trend functions do not exist.

- [ ] **Step 3: Add detail panel markup and responsive styles**

Place the panel directly after the selected stream row with a selected key header, close button, four compact metric summaries, and four inline SVG charts containing grid lines, `chart-area-*`, and `chart-path-*`. Keep each chart at a fixed low height and add desktop/mobile CSS matching the approved demo direction.

- [ ] **Step 4: Implement one-second selected-stream polling**

Add state for the selected key, generation, previous cumulative frame counts, per-metric arrays, an `AbortController`, and a timer. Poll `/api/v1/streams/{encoded key}` every second only while the Streams view and document are visible. Derive video/audio FPS from cumulative frame deltas; use `stats.bitrate_kbps` and `gop_duration_ms`. Reset arrays when generation changes, selected key disappears, the panel closes, or the view is hidden.

- [ ] **Step 5: Implement four-chart SVG scaling**

Render all four series into stable low-height viewBoxes such as `0 0 720 82`, scale each series to its min/max with a one-unit floor, update each current/min/max label, and show an explicit no-data state. Use the existing `newActiveViewSignal` lifecycle so hidden tabs abort trend requests.

- [ ] **Step 6: Run focused tests and the documentation check**

Run: `go test ./module/api -run '^TestConsole.*Trend|^TestConsoleProtocolLabMediaAndCacheRendering$' -count=1`

Run: `tools/check-agent-docs_test.sh`

Expected: PASS and the agent documentation check reports success.

- [ ] **Step 7: Commit**

```bash
git add module/api/console.html module/api/console_management_test.go
git commit -m "feat: add selected stream trend panel"
```

### Task 4: Normalize the visual system across all Console views

**Files:**
- Modify: `module/api/console.html` (shared CSS tokens and page markup classes)
- Test: `module/api/console_management_test.go`

**Interfaces:**
- Existing view IDs, permission attributes, action identifiers, and form field IDs remain stable.
- Shared classes cover page headings, toolbars, cards, tables, forms, status badges, empty/error states, and buttons.

- [ ] **Step 1: Add failing shared-style assertions**

Extend the console DOM test to verify every management view contains the shared page shell classes and that each operational table uses the stable table wrapper.

- [ ] **Step 2: Run the focused DOM test to verify it fails**

Run: `go test ./module/api -run '^TestConsoleManagementViewsExposeSupportedControlPlanes$' -count=1`

Expected: the current views rely on mixed inline styles and do not expose the shared class contract.

- [ ] **Step 3: Add the token and component CSS layer**

Define shared variables and classes for surfaces, text roles, focus, buttons, stat cards, toolbars, section headers, tables, forms, badges, error/empty states, and responsive behavior. Keep the approved mint/cyan/amber/coral palette and preserve reduced-motion rules.

- [ ] **Step 4: Apply shared classes without changing view behavior**

Update GB28181, Config, Cluster, SIP Calls, Storage, Security, and Streams markup to use the shared classes. Remove only conflicting inline presentation styles; do not alter API calls, permission checks, action identifiers, or form submission handlers.

- [ ] **Step 5: Run focused DOM and browser tests**

Run: `go test ./module/api -run '^TestConsoleManagementViewsExposeSupportedControlPlanes$|^TestConsole.*Polling|^TestConsole.*Rendering' -count=1`

Expected: PASS with all existing view behavior and IDs intact.

- [ ] **Step 6: Commit**

```bash
git add module/api/console.html module/api/console_management_test.go
git commit -m "feat: unify console visual system"
```

### Task 5: Turn WHIP publish into the dedicated workspace

**Files:**
- Modify: `module/api/console.html` (publish markup, CSS, and initialization)
- Test: `module/api/console_publish_test.go`
- Test: `module/api/console_management_test.go`

**Interfaces:**
- `/console/publish` sets a route marker, hides the management shell, and shows `publish-workspace`.
- `openPublishModal()` becomes a route navigation helper for backwards-compatible callers.
- Existing `startPublish`, `stopPublish`, and stats functions target the workspace elements.

- [ ] **Step 1: Update the browser test to the publish route**

Change `TestConsolePublishFlow` to navigate to `serverURL + "/console/publish"`, wait for `#publish-workspace`, and keep the existing device enumeration, start, connected, and cleanup assertions.

- [ ] **Step 2: Run the browser test to verify it fails**

Run: `go test ./module/api -run '^TestConsolePublishFlow$' -count=1`

Expected: the route is present but the current page has no full-page workspace and the test's workspace selector is missing.

- [ ] **Step 3: Replace the modal shell with approved workspace markup**

Add a route marker before first paint, wrap the management shell in `#console-app`, and add `#publish-workspace` with the two-column preview/control layout, back link, stream key, camera, microphone, codec, connection state, start/stop buttons, and stats strip. Remove the publish overlay from the active flow and preserve stable IDs used by WHIP logic.

- [ ] **Step 4: Make the page route-aware and lifecycle-safe**

Initialize device preview only on the publish route. Make the Console header Publish button navigate to `/console/publish`. Ensure stop/failure returns to the ready state, stops local tracks, clears stats, and never starts management polling on the publish route. Keep the existing secure-context, WHIP offer/answer, ICE, and DELETE behavior.

- [ ] **Step 5: Run the browser test to verify it passes**

Run: `go test ./module/api -run '^TestConsolePublishFlow$' -count=1`

Expected: PASS when Chromium and fake media devices are available; otherwise the test reports its existing environment skip.

- [ ] **Step 6: Run console DOM tests**

Run: `go test ./module/api -run '^TestConsoleManagementViewsExposeSupportedControlPlanes$|^TestConsole.*Polling|^TestConsole.*Publish' -count=1`

Expected: PASS with the new workspace markup and existing tab behavior intact.

- [ ] **Step 7: Commit**

```bash
git add module/api/console.html module/api/console_publish_test.go module/api/console_management_test.go
git commit -m "feat: add dedicated whip publish workspace"
```

### Task 6: Synchronize user and AI-facing documentation

**Files:**
- Modify: `README.md`
- Modify: `README.zh-CN.md`
- Modify: `llms.txt`
- Modify: `llms-full.txt`

**Interfaces:**
- Documentation states that `/console/publish` is the supported browser WHIP workflow and that Streams trends are selected on demand with a 60-second/1-second client window.

- [ ] **Step 1: Update the quick-start documentation**

Add the new route and describe the stable list/detail workflow in the existing console sections in English and Chinese. Keep the existing security warning about development credentials and secure contexts.

- [ ] **Step 2: Update AI-facing navigation and full facts**

Add the route and client-side trend behavior to the console capability bullets in `llms.txt` and the corresponding console/WebRTC sections in `llms-full.txt`. Do not claim server-side historical storage.

- [ ] **Step 3: Run documentation validation**

Run: `tools/check-agent-docs_test.sh`

Expected: `agent-docs: all checks passed` and `agent documentation check passed`.

- [ ] **Step 4: Commit**

```bash
git add README.md README.zh-CN.md llms.txt llms-full.txt
git commit -m "docs: describe console publish workspace and trends"
```

### Task 7: Full verification

**Files:**
- Test: `module/api` and repository test suites

- [ ] **Step 1: Run focused API tests**

Run: `go test ./module/api -count=1`

- [ ] **Step 2: Run the quick package suite**

Run: `go test ./...`

- [ ] **Step 3: Run the documentation contract test**

Run: `tools/check-agent-docs_test.sh`

- [ ] **Step 4: Inspect the final diff**

Run: `git diff --check` and `git status --short`.

Expected: no whitespace errors, only scoped source/tests/docs changes, and the demo artifact remains clearly under `docs/superpowers/demos/`.
