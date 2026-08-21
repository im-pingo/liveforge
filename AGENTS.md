# LiveForge Agent Contract

This file is the repository contract for coding agents, including Codex, Claude Code, and other automated contributors.

## Read before changing code

1. Read `agent-manifest.json` for the current machine-readable project facts.
2. Read `llms.txt` for the short navigation map.
3. Read `llms-full.txt` and the linked recipe when the task changes runtime behavior, build requirements, an API, or configuration.
4. Inspect the relevant source and tests. Do not infer support from names, stale plans, or an unverified README claim.

## Documentation synchronization is mandatory

Every source change must include an explicit documentation impact check. A behavior, protocol, API, configuration, build, dependency, security, or release change must update the corresponding AI-facing documentation in the same change:

- Update `agent-manifest.json` when capabilities, prerequisites, ports, install methods, security posture, support status, or verification commands change.
- Update `llms-full.txt` when a user or agent needs new project facts or a new workflow.
- Update `llms.txt` when a top-level project description or discovery link changes.
- Update `README.md` and `README.zh-CN.md` for user-visible behavior and quick starts.
- Update `docs/api/openapi.yaml` for REST or WebRTC HTTP contract changes.
- Update `docs/config/config.schema.json` and a recipe for configuration changes.
- Add or update a recipe under `docs/recipes/` for a new supported workflow.
- Update `AGENTS.md` when the build, test, review, or documentation contract changes.

Do not claim a feature is supported until there is a source implementation and a passing verification path. Mark unavailable Docker images, unreleased binaries, experimental features, and optional FFmpeg support explicitly.

## Build and test rules

- The module requires Go 1.26 or newer, as declared by `go.mod`.
- The baseline build and test use the `audiocodec` build tag and require FFmpeg development libraries:
  `CGO_ENABLED=1 go build -tags audiocodec ./cmd/liveforge`
  `CGO_ENABLED=1 go test -tags audiocodec -race -coverprofile=coverage.out -covermode=atomic ./...`
- The no-native-dependency quick package check is `go test ./...`; it skips FFmpeg-tagged transcoding integration tests.
- A build without the `audiocodec` tag is valid for deployments that do not need audio transcoding. It must not be described as providing audio transcoding.
- Release binaries are built with the portable no-CGO profile; audio transcoding is available in source builds with FFmpeg or in the tagged Docker image.
- Run `tools/check-agent-docs_test.sh` after every change. Run `tools/check-agent-docs.sh` with `CHECK_AGENT_DOCS_DIFF=1` in CI or before opening a pull request.
- Run focused package tests for the changed module, then the tagged baseline suite when the environment has Go 1.26 and FFmpeg available.

## Safety and review rules

- The sample configuration is for local development. It disables authentication and TLS and uses the development console credentials `admin/admin`; never recommend it for public exposure.
- Never commit secrets, tokens, private URLs, recordings, generated binaries, or local configuration.
- Keep destructive API operations such as stream deletion and publisher kicking out of automated workflows unless the user explicitly requests them.
- Prefer a versioned release or a commit SHA over `latest`. Verify that an image, tag, or release asset exists before documenting it as available.
- Preserve the repository's existing module boundaries and add tests for behavior changes.

## Pull request checklist

- [ ] Source behavior and tests are updated.
- [ ] The relevant AI-facing documents are updated in the same change.
- [ ] `agent-manifest.json` is valid and agrees with `go.mod`.
- [ ] `tools/check-agent-docs_test.sh` passes.
- [ ] Build, focused tests, and baseline tests were run or their environment blocker is recorded.
- [ ] Security, optional dependencies, and release availability are stated accurately.
