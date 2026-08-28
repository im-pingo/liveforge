# Main CI Green Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make post-merge `main` CI reliable by fixing GB28181 ephemeral-port allocation, making lint deterministic, and removing only confirmed-unused branches.

**Architecture:** When `NewRTPReceiver` receives port zero, it will derive the RTCP port from the RTP socket's assigned port. CI will pin golangci-lint; the action's `only-new-issues` mode already compares a pull request with its patch and a main push with that push's commit diff, so an historical lint backlog will not block later incremental changes while new issues remain gated.

**Tech Stack:** Go 1.26, FFmpeg-tagged Go tests, GitHub Actions, golangci-lint.

**Spec:** `docs/TECHNICAL-RISKS.md` and `AGENTS.md`.

## Global Constraints

- Use Go 1.26 or newer and the repository's `audiocodec` build tag for the tagged baseline.
- Run `tools/check-agent-docs_test.sh` after every source change.
- Never commit secrets, generated binaries, recordings, or local configuration.
- Use author identity `im-pingo` for commits.
- Preserve `main`, the active `codex/webrtc-playback-fix` branch, and unmerged work unless explicitly authorized for deletion.

---

### Task 1: Fix GB28181 ephemeral RTCP port allocation

**Files:**
- Modify: `module/gb28181/rtp_receiver.go:38-57`
- Test: `module/gb28181/rtp_receiver_test.go`

**Interfaces:** `NewRTPReceiver(port int, publisher *Publisher)` keeps its public signature; port zero must result in RTCP listening on the assigned RTP port plus one.

- [ ] **Step 1: Add the failing regression test.** Add `TestNewRTPReceiverUsesAssignedPortForRTCP`, call `NewRTPReceiver(0, NewPublisher("ephemeral-port", nil))`, defer `receiver.Close`, read `receiver.LocalPort()` and `receiver.rtcpConn.LocalAddr().(*net.UDPAddr).Port`, assert RTP is positive and RTCP equals RTP plus one.
- [ ] **Step 2: Run `go test -tags audiocodec ./module/gb28181 -run TestNewRTPReceiverUsesAssignedPortForRTCP -count=1` and verify it fails because the current code attempts UDP port 1.
- [ ] **Step 3: After the RTP bind succeeds, compute RTCP port from the actual RTP local port when the requested port is zero; retain `port + 1` for explicitly allocated ports; close RTP if RTCP binding fails.
- [ ] **Step 4: Run `go test -tags audiocodec ./module/gb28181 -run 'TestNewRTPReceiverUsesAssignedPortForRTCP|TestRTPReceiver|TestMediaSessionCloseOwnsReceiverAndAllowsPortReuse' -count=1` and then `go test -tags audiocodec ./module/gb28181 -count=1`.
- [ ] **Step 5: Commit with `git add module/gb28181/rtp_receiver.go module/gb28181/rtp_receiver_test.go && git commit -m "fix: derive GB28181 RTCP port from ephemeral RTP port"`.

### Task 2: Make lint deterministic on PRs and main pushes

**Files:**
- Modify: `.github/workflows/ci.yml:34-39`

**Interfaces:** The `Lint` job must remain a required quality gate for pull requests and must check only the new diff on `main` pushes.

- [ ] **Step 1: Pin `golangci-lint-action` to the repository's Node 24-compatible action major and replace `version: latest` with the verified `v2.13.2` release; retain `only-new-issues: true`, which the action maps to the PR patch or push commit diff automatically.
- [ ] **Step 2: Validate the workflow diff with `git diff --check` and inspect `.github/workflows/ci.yml` for valid YAML and no unrelated changes.
- [ ] **Step 3: Commit with `git add .github/workflows/ci.yml && git commit -m "ci: lint only changes on main pushes"`.

### Task 3: Documentation and full verification

**Files:** Inspect `agent-manifest.json`, `llms.txt`, `llms-full.txt`, `README.md`, `README.zh-CN.md`, `docs/TECHNICAL-RISKS.md`, and `docs/PROGRESS.md`; modify only if the final behavior or CI contract changes a documented fact.

- [ ] **Step 1: Confirm the port fix changes no public API, configuration, prerequisite, or supported protocol; document any changed CI version or workflow fact in the required AI-facing files.
- [ ] **Step 2: Run `go test ./...`, `go test -tags audiocodec ./module/gb28181 -count=1`, `tools/check-agent-docs_test.sh`, `CHECK_AGENT_DOCS_DIFF=1 tools/check-agent-docs.sh`, `git diff --check`, and `jq empty agent-manifest.json`.
- [ ] **Step 3: When FFmpeg development libraries are available, run `CGO_ENABLED=1 go build -tags audiocodec ./cmd/liveforge` and `CGO_ENABLED=1 go test -tags audiocodec -race -coverprofile=coverage.out -covermode=atomic ./...`.
- [ ] **Step 4: Push the fix branch, create or update its PR, wait for all checks, merge only after they pass, then verify the post-merge `main` run has successful Agent Documentation, Lint, Test, Security Scan, and Docker Build jobs.

### Task 4: Delete confirmed-unused branches

**Files:** Git refs only.

- [ ] **Step 1: Delete local and remote `codex/liveforge-completion` and `codex/p0-p1-main-playback`; both corresponding PRs are merged.
- [ ] **Step 2: Delete local-only stale `codex/forwarding-performance-optimization` and `playback-startup-fixes`; their remote refs are already gone.
- [ ] **Step 3: Preserve `fix/p0-p1-hardening` because PR #16 was closed without merge and its commits may not exist in main; delete it only after explicit disposal authorization.
- [ ] **Step 4: Verify with `git fetch origin --prune`, `git branch -vv --all`, and `gh api repos/im-pingo/liveforge/branches --paginate --jq '.[].name'`; `main` and `codex/webrtc-playback-fix` must remain.
