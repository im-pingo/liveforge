# Docker Deployment Design

**Date**: 2026-04-14
**Status**: Approved

## Goal

Provide a simple, one-command Docker deployment for LiveForge, published to Docker Hub as `impingo/liveforge`.

## Approach

Debian Bookworm multi-stage build with system FFmpeg shared libraries.

## Dockerfile

### Stage 1: Builder (`golang:1.26-bookworm`)

- Install build deps: `pkg-config`, `libavcodec-dev`, `libswresample-dev`, `libavutil-dev`
- Copy `go.mod` and `go.sum` first for layer caching
- Run `go mod download`
- Copy full source
- Build: `CGO_ENABLED=1 go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o /liveforge ./cmd/liveforge`
- Version injected via `--build-arg VERSION`

### Stage 2: Runtime (`debian:bookworm-slim`)

- Install runtime-only packages: `libavcodec60`, `libswresample4`, `libavutil58`, `ca-certificates`
- Copy binary from builder
- Copy default config to `/etc/liveforge/liveforge.yaml`
- Create `/data` directory for recordings
- Expose ports: `1935 8554 8080 8443 6000 5060/udp 8090 9090`
- Entrypoint: `/usr/local/bin/liveforge -c /etc/liveforge/liveforge.yaml`

## docker-compose.yaml

Minimal compose file for quick deployment:

```yaml
services:
  liveforge:
    image: impingo/liveforge:latest
    ports:
      - "1935:1935"     # RTMP
      - "8554:8554"     # RTSP
      - "8080:8080"     # HTTP (HLS/DASH/FLV)
      - "8443:8443"     # WebRTC
      - "6000:6000"     # SRT
      - "5060:5060/udp" # SIP
      - "8090:8090"     # API + Console
    volumes:
      - ./configs/liveforge.yaml:/etc/liveforge/liveforge.yaml:ro
      - liveforge-data:/data
    restart: unless-stopped

volumes:
  liveforge-data:
```

## .dockerignore

Exclude: `.git`, `bin/`, `.claude/`, `liveforge-*` binaries, macOS vendored libs, test fixtures, IDE files.

## Documentation Updates

Add "Docker" subsection to Quick Start in both `README.md` and `README.zh-CN.md`:

1. `docker run` one-liner
2. `docker-compose` usage
3. Custom config via volume mount

## Multi-arch Build

Support `linux/amd64` and `linux/arm64` via `docker buildx`.

## Image Tags

- `impingo/liveforge:latest` — latest release
- `impingo/liveforge:<version>` — specific version from git tag

## Files to Create/Modify

| File | Action |
|------|--------|
| `Dockerfile` | Create |
| `.dockerignore` | Create |
| `docker-compose.yaml` | Create |
| `README.md` | Add Docker section |
| `README.zh-CN.md` | Add Docker section |
