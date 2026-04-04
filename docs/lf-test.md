# lf-test

`lf-test` is LiveForge's integration testing CLI. It pushes media, subscribes to streams, validates AV quality, tests authentication, and orchestrates multi-node cluster scenarios -- all from a single binary.

## Quick Start

```bash
# Build
go build -o bin/lf-test ./tools/lf-test/

# Push a test stream (uses embedded FLV source)
./bin/lf-test push --protocol rtmp --target rtmp://127.0.0.1:1935/live/test --duration 5s

# Play and assert video quality
./bin/lf-test play --protocol rtmp --url rtmp://127.0.0.1:1935/live/test \
  --duration 5s \
  --assert "video.fps>=29" \
  --assert "video.dts_monotonic==true"
```

Exit codes: `0` = pass, `1` = assertion failure, `2` = error.

---

## Command Reference

### push

Push a test media stream to a LiveForge server.

```
lf-test push [flags]
```

| Flag | Default | Description |
|------|---------|-------------|
| `--protocol` | `rtmp` | Push protocol: `rtmp`, `rtsp`, `srt`, `whip`, `gb28181` |
| `--target` | *(required)* | Target URL (e.g. `rtmp://host:1935/live/test`) |
| `--duration` | `0` | Push duration (e.g. `5s`, `1m`); `0` = until source exhausted |
| `--token` | | Auth token to include in the connection |
| `--assert` | | Assertion expression (repeatable) |
| `--output` | auto | Output format: `human`, `json` (auto-detects from TTY) |
| `--timeout` | `30s` | Overall timeout |

**Example:**

```bash
./bin/lf-test push \
  --protocol srt \
  --target "srt://127.0.0.1:6000?streamid=publish:live/test" \
  --duration 10s \
  --assert "push.frames_sent>0"
```

### play

Subscribe to a stream and analyze AV quality.

```
lf-test play [flags]
```

| Flag | Default | Description |
|------|---------|-------------|
| `--protocol` | `rtmp` | Play protocol: `rtmp`, `rtsp`, `srt`, `whep`, `httpflv`, `wsflv`, `hls`, `llhls`, `dash` |
| `--url` | *(required)* | Stream URL |
| `--duration` | `0` | Play duration; `0` = until server closes |
| `--token` | | Auth token |
| `--assert` | | Assertion expression (repeatable) |
| `--output` | auto | Output format: `human`, `json` |
| `--timeout` | `30s` | Overall timeout |

**Example:**

```bash
./bin/lf-test play \
  --protocol httpflv \
  --url "http://127.0.0.1:8080/live/test.flv" \
  --duration 5s \
  --assert "video.codec==H264" \
  --assert "audio.sample_rate>=44100" \
  --assert "sync.max_drift_ms<100"
```

### auth

Run an authentication test matrix against a running server with JWT auth enabled.

```
lf-test auth [flags]
```

| Flag | Default | Description |
|------|---------|-------------|
| `--secret` | *(required)* | JWT secret for generating test tokens |
| `--stream` | `live/test` | Stream key for test cases |
| `--server-rtmp` | | RTMP server address (e.g. `127.0.0.1:1935`) |
| `--server-srt` | | SRT server address (e.g. `127.0.0.1:6000`) |
| `--server-http` | | HTTP server address (e.g. `127.0.0.1:8080`) |
| `--assert` | | Assertion expression (repeatable) |
| `--output` | auto | Output format: `human`, `json` |
| `--timeout` | `60s` | Overall timeout |

At least one `--server-*` flag is required. The test generates 6 JWT test cases per protocol/action combination:

| Case | Description | Expected |
|------|-------------|----------|
| `valid` | Correct secret, matching stream/action, future expiry | Allow |
| `expired` | Correct secret but expiry in the past | Deny |
| `wrong_secret` | Invalid HMAC-SHA256 secret | Deny |
| `missing` | No token provided | Deny |
| `wrong_stream` | Token for a different stream key | Deny |
| `wrong_action` | Token for wrong action (e.g. subscribe when publishing) | Deny |

**Example:**

```bash
./bin/lf-test auth \
  --secret "my-jwt-secret" \
  --server-rtmp 127.0.0.1:1935 \
  --server-http 127.0.0.1:8080 \
  --assert "auth.passed>=10" \
  --output json
```

### cluster

Run a multi-node cluster test. Starts liveforge server processes, pushes to origin, verifies relay to edge.

```
lf-test cluster [flags]
```

| Flag | Default | Description |
|------|---------|-------------|
| `--topology` | `origin-edge` | Topology name (see below) |
| `--push-protocol` | `rtmp` | Protocol for pushing to origin |
| `--play-protocol` | `rtmp` | Protocol for playing from edge |
| `--relay-protocol` | `rtmp` | Protocol for inter-node relay |
| `--stream` | `live/test` | Stream key |
| `--duration` | `5s` | Play duration |
| `--edges` | `1` | Edge node count (only for `origin-multi-edge`) |
| `--binary` | auto | Path to liveforge binary |
| `--assert` | | Assertion expression (repeatable) |
| `--output` | auto | Output format: `human`, `json` |
| `--timeout` | `120s` | Overall timeout |

**Example:**

```bash
./bin/lf-test cluster \
  --topology origin-edge \
  --push-protocol rtmp \
  --play-protocol rtmp \
  --relay-protocol rtmp \
  --duration 5s \
  --assert "cluster.relay_ms<1000" \
  --assert "video.fps>=29"
```

---

## Assertion Syntax

Assertions use the format: `field.path operator value`

**Operators:** `>=`, `<=`, `>`, `<`, `==`, `!=`

### Field Paths

Fields use JSON tag names with dot notation. The first segment selects the sub-report.

#### Play Fields (prefix: `video.`, `audio.`, `sync.`, `codec.`)

| Path | Type | Description |
|------|------|-------------|
| `video.codec` | string | Video codec name (e.g. `H264`, `H265`) |
| `video.profile` | string | Codec profile |
| `video.level` | string | Codec level |
| `video.resolution` | string | Resolution (e.g. `1920x1080`) |
| `video.fps` | float | Frames per second |
| `video.bitrate_kbps` | float | Video bitrate in kbps |
| `video.keyframe_interval` | float | Seconds between keyframes |
| `video.dts_monotonic` | bool | Whether DTS timestamps are monotonic |
| `video.frame_count` | int | Total video frames received |
| `audio.codec` | string | Audio codec name (e.g. `AAC`) |
| `audio.sample_rate` | int | Audio sample rate in Hz |
| `audio.channels` | int | Audio channel count |
| `audio.bitrate_kbps` | float | Audio bitrate in kbps |
| `audio.dts_monotonic` | bool | Whether audio DTS is monotonic |
| `audio.frame_count` | int | Total audio frames received |
| `sync.max_drift_ms` | float | Maximum A/V sync drift in ms |
| `sync.avg_drift_ms` | float | Average A/V sync drift in ms |
| `codec.video_match` | bool | Video codec params match source |
| `codec.audio_match` | bool | Audio codec params match source |

#### Push Fields (prefix: `push.`)

| Path | Type | Description |
|------|------|-------------|
| `push.protocol` | string | Protocol used |
| `push.target` | string | Target URL |
| `push.duration_ms` | int | Push duration in ms |
| `push.frames_sent` | int | Total frames sent |
| `push.bytes_sent` | int | Total bytes sent |

#### Auth Fields (prefix: `auth.`)

| Path | Type | Description |
|------|------|-------------|
| `auth.total` | int | Total test cases |
| `auth.passed` | int | Passed test cases |
| `auth.failed` | int | Failed test cases |

#### Cluster Fields (prefix: `cluster.`)

| Path | Type | Description |
|------|------|-------------|
| `cluster.topology` | string | Topology name |
| `cluster.relay_ms` | int | Stream relay latency in ms |

### Type Rules

- **Numeric** (int, float): all operators supported
- **String**: only `==` and `!=`
- **Boolean**: only `==` and `!=`, values are `true`/`false`

### Examples

```bash
# Video quality checks
--assert "video.fps>=29"
--assert "video.dts_monotonic==true"
--assert "video.frame_count>100"

# Audio checks
--assert "audio.codec==AAC"
--assert "audio.sample_rate>=44100"

# Sync checks
--assert "sync.max_drift_ms<100"

# Push checks
--assert "push.frames_sent>0"

# Auth checks
--assert "auth.failed==0"

# Cluster checks
--assert "cluster.relay_ms<1000"
```

---

## Cluster Topologies

### Built-in Topologies

#### `origin-edge`

Two nodes. Origin receives the published stream and forwards to edge.

```
[origin] ──relay──> [edge]
```

#### `origin-multi-edge`

One origin with N edge nodes. Use `--edges` to set the count.

```
            ┌──relay──> [edge-1]
[origin] ───┼──relay──> [edge-2]
            └──relay──> [edge-3]
```

#### `origin-center-edge`

Three-tier relay. Origin forwards to center, center forwards to edge.

```
[origin] ──relay──> [center] ──relay──> [edge]
```

### How Cluster Tests Work

1. Allocate ephemeral ports for all nodes
2. Generate YAML configs with forward/origin cluster settings
3. Start node processes (origin first, then centers, then edges)
4. Wait for all nodes to report healthy via `/api/v1/server/health`
5. Push a test stream to the origin node
6. Wait for stream propagation to edge via `/api/v1/streams`
7. Play from edge and analyze AV quality
8. Stop all processes (SIGTERM, then SIGKILL after 5s)
9. Return combined report

### Binary Detection

The cluster command needs a compiled liveforge binary. It searches in order:

1. `--binary` flag
2. `$LF_BINARY` environment variable
3. `./bin/liveforge` (relative to CWD)
4. `$GOPATH/bin/liveforge`

---

## CI/CD Integration

### GitHub Actions

```yaml
name: Stream Integration Tests
on: [push, pull_request]

jobs:
  integration:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4

      - uses: actions/setup-go@v5
        with:
          go-version: '1.22'

      - name: Build server and test tool
        run: |
          go build -o bin/liveforge ./cmd/liveforge
          go build -o bin/lf-test ./tools/lf-test/

      - name: Start LiveForge
        run: |
          ./bin/liveforge -c configs/test.yaml &
          sleep 2  # wait for server startup

      - name: Push + Play test
        run: |
          ./bin/lf-test push --protocol rtmp \
            --target rtmp://127.0.0.1:1935/live/ci \
            --duration 5s &
          sleep 1
          ./bin/lf-test play --protocol rtmp \
            --url rtmp://127.0.0.1:1935/live/ci \
            --duration 3s \
            --assert "video.fps>=25" \
            --assert "video.dts_monotonic==true" \
            --output json

      - name: Cluster test
        run: |
          ./bin/lf-test cluster \
            --topology origin-edge \
            --duration 5s \
            --assert "cluster.relay_ms<2000" \
            --output json
```

### Shell Script Pattern

```bash
#!/bin/bash
set -euo pipefail

LF_TEST="./bin/lf-test"
PASS=0
FAIL=0

run_test() {
    local name="$1"
    shift
    echo "=== $name ==="
    if "$LF_TEST" "$@" --output human; then
        PASS=$((PASS + 1))
    else
        FAIL=$((FAIL + 1))
    fi
    echo
}

run_test "RTMP push+play" play --protocol rtmp \
    --url rtmp://127.0.0.1:1935/live/test \
    --duration 5s --assert "video.fps>=29"

run_test "HLS play" play --protocol hls \
    --url http://127.0.0.1:8080/live/test.m3u8 \
    --duration 5s --assert "video.frame_count>50"

run_test "Auth matrix" auth --secret "$JWT_SECRET" \
    --server-rtmp 127.0.0.1:1935 \
    --assert "auth.failed==0"

echo "Results: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ] || exit 1
```

---

## AI Agent Guide

### JSON Output Schema

All commands produce a `TopLevelReport` when using `--output json`:

```json
{
  "command": "play",
  "timestamp": "2024-01-01T00:00:00Z",
  "duration_ms": 5000,
  "pass": true,
  "play": {
    "video": {
      "codec": "H264",
      "profile": "High",
      "level": "4.1",
      "resolution": "1920x1080",
      "fps": 30.0,
      "bitrate_kbps": 2500.0,
      "keyframe_interval": 2.0,
      "dts_monotonic": true,
      "frame_count": 150
    },
    "audio": {
      "codec": "AAC",
      "sample_rate": 44100,
      "channels": 2,
      "bitrate_kbps": 128.0,
      "dts_monotonic": true,
      "frame_count": 200
    },
    "sync": {
      "max_drift_ms": 15.5,
      "avg_drift_ms": 8.2
    },
    "stalls": [],
    "codec": {
      "video_match": true,
      "audio_match": true
    },
    "duration_ms": 5000
  },
  "errors": []
}
```

### Exit Code Interpretation

| Code | Meaning | Agent Action |
|------|---------|-------------|
| `0` | All assertions passed | Proceed |
| `1` | One or more assertions failed | Inspect JSON for which assertions failed |
| `2` | Infrastructure error (connection refused, binary not found) | Check server status, configuration |

### Troubleshooting Flows

**Push succeeds but play receives no frames:**
1. Verify the stream key matches between push and play
2. Check the play protocol matches what the server has enabled
3. For cluster tests, check relay configuration

**Auth test cases all fail:**
1. Verify `--secret` matches the server's configured JWT secret
2. Check the server has auth middleware enabled
3. Verify the stream key format matches expectations

**Cluster health check times out:**
1. Verify the liveforge binary path is correct
2. Check that the required ports are available
3. Increase `--timeout` for slower environments

**Relay latency is very high:**
1. This can indicate network or CPU contention
2. Try with fewer edge nodes
3. Check if the relay protocol is the bottleneck (try RTMP vs SRT)

---

## Troubleshooting

### Connection Refused

```
Error: CONNECT_FAILED: dial tcp 127.0.0.1:1935: connection refused
```

The server is not running or not listening on the expected port. Start the server first, then run lf-test.

### No Video Frames Received

```
FAIL  video.frame_count>0
```

Possible causes:
- The stream is not being published (push hasn't started or has already ended)
- The play protocol doesn't match the server configuration
- The stream key is wrong
- For HLS/DASH: the server needs time to generate initial segments (add a delay between push start and play)

### Auth Rejected (Expected Allow)

```
[FAIL] rtmp/publish  expect=true actual=false
```

The server rejected a token that should be valid. Check:
- The `--secret` matches the server's `auth.secret` config
- The server's auth middleware is configured for the tested protocol
- The JWT claims format matches (`sub`, `action`, `exp`)

### Cluster Binary Not Found

```
Error: liveforge binary not found
Hint: set --binary or $LF_BINARY, or build to ./bin/liveforge
```

Build the server first:

```bash
go build -o bin/liveforge ./cmd/liveforge
```

Or set the path explicitly:

```bash
./bin/lf-test cluster --binary /path/to/liveforge --topology origin-edge
```

### Timeout

```
Error: TIMEOUT: context deadline exceeded
```

The operation took longer than `--timeout`. Increase the timeout value or check that the server is responsive. For cluster tests, the default is 120s which should be sufficient for most topologies.

---

## Go Library Usage

The `tools/testkit/` packages can be used directly as a Go library for custom test scenarios:

```go
package mytest

import (
    "context"
    "testing"
    "time"

    "github.com/im-pingo/liveforge/tools/testkit/analyzer"
    "github.com/im-pingo/liveforge/tools/testkit/play"
    "github.com/im-pingo/liveforge/tools/testkit/push"
    "github.com/im-pingo/liveforge/tools/testkit/source"
)

func TestMyStream(t *testing.T) {
    ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()

    // Push
    pusher, _ := push.NewPusher("rtmp")
    src := source.NewFLVSourceLoop(1)
    cfg := push.PushConfig{
        Protocol: "rtmp",
        Target:   "rtmp://127.0.0.1:1935/live/test",
        Duration: 5 * time.Second,
    }
    go pusher.Push(ctx, src, cfg)

    time.Sleep(1 * time.Second) // wait for stream

    // Play + Analyze
    player, _ := play.NewPlayer("rtmp")
    az := analyzer.New()
    playCfg := play.PlayConfig{
        Protocol: "rtmp",
        URL:      "rtmp://127.0.0.1:1935/live/test",
        Duration: 3 * time.Second,
    }
    player.Play(ctx, playCfg, az.Feed)

    report := az.Report()
    if report.Video.FPS < 25 {
        t.Errorf("FPS too low: %.2f", report.Video.FPS)
    }
}
```

### Available Packages

| Package | Description |
|---------|-------------|
| `tools/testkit/source` | Embedded FLV test media with looping |
| `tools/testkit/push` | Multi-protocol push clients |
| `tools/testkit/play` | Multi-protocol play/subscribe clients |
| `tools/testkit/analyzer` | AV quality analysis (FPS, bitrate, sync, stalls) |
| `tools/testkit/report` | Report structs, JSON/human formatters, assertion engine |
| `tools/testkit/auth` | JWT generation, test case matrix, protocol probes |
| `tools/testkit/cluster` | Topology, config generation, process orchestration |
| `tools/testkit/testutil` | Integration test helpers (server startup, config) |
