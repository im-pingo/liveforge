# Build From Source

## Use when

Use this path when a released binary or container image is not available, or when the deployment needs a custom build profile.

## Requirements

- Go 1.26 or newer.
- For the default build: Go module download access.
- For audio transcoding: CGO and FFmpeg development libraries `libavcodec`, `libswresample`, and `libavutil`.

## Default build

```bash
go build -trimpath -ldflags "-s -w" -o bin/liveforge ./cmd/liveforge
./bin/liveforge -c configs/liveforge.yaml
```

This profile does not enable audio transcoding.

## Audio transcoding build

```bash
CGO_ENABLED=1 go build -trimpath -tags audiocodec -ldflags "-s -w" -o bin/liveforge ./cmd/liveforge
./bin/liveforge -c configs/liveforge.yaml
```

## Verify

```bash
curl http://127.0.0.1:8090/api/v1/server/health
```

Expected response data contains `status: healthy`. Do not expose the sample configuration to the public internet.
