# Release Verification

The checked-in sample configuration is for local development only: it disables TLS and authentication and uses the console credentials `admin/admin`. Never expose it publicly unchanged.

## Availability Contract

Source builds are always available from a checked-out commit. Versioned binaries and `ghcr.io/im-pingo/liveforge:<version>` exist only after a `v*` tag triggers a successful Release workflow. The GHCR package must be public for anonymous pulls or the client must run `docker login ghcr.io`. Verify existence before documenting or deploying an artifact, and prefer a version or commit SHA over `latest`.

Release binaries use `CGO_ENABLED=0`; they do not contain audio transcoding. The Dockerfile builds with CGO, `-tags audiocodec`, and FFmpeg libraries. A source build provides audio transcoding only when it uses that tagged profile and the FFmpeg development libraries are available.

## Pre-Release Gates

Prerequisites are Go 1.26+, FFmpeg development libraries for the tagged baseline, Docker/Buildx for container verification, and `gh` only when checking published GitHub state.

```bash
tools/check-agent-docs_test.sh
CHECK_AGENT_DOCS_DIFF=1 tools/check-agent-docs.sh
go test ./...
CGO_ENABLED=1 go build -tags audiocodec ./cmd/liveforge
CGO_ENABLED=1 go test -tags audiocodec -race \
  -coverprofile=coverage.out -covermode=atomic ./...
```

Run the bounded performance and capability gates as part of the same release
review. They exercise the real Stream/ring/GOP ingress path, cached bitrate
admission, RTMP/RTSP framing, relay accounting, local UDP RTP output, and the
shared FFmpeg transcode reader fanout:

```bash
go test -run '^$' -bench 'BenchmarkStreamIngressProduction|BenchmarkStreamIngressWithBitrateLimit|BenchmarkRTMPRelaySendMediaFrameProduction|BenchmarkRTSPRelaySendFrameProduction|BenchmarkRelayObservationAccounting' -benchmem -count=3 ./core ./module/cluster
go test -run '^$' -bench 'BenchmarkGBOutboundSendFrame|BenchmarkSIPOutboundSendFrame' -benchmem -count=3 ./module/gb28181 ./module/sipgateway
go test -tags audiocodec -run '^$' -bench '^BenchmarkTranscodeReaderFanoutAdmission$' -benchmem -count=3 ./core
go test -tags audiocodec ./pkg/audiocodec ./module/record ./module/sipgateway ./module/gb28181 ./module/webrtc -run 'Codec|codec|Audio|audio|WHEP|WHIP' -count=1
```

These measurements and tests are bounded regression evidence. Socket,
kernel, network, subscriber-count, and deployment-capacity limits remain
target-environment concerns; record the target-platform result rather than
turning a local benchmark into a universal throughput claim.

Every command must exit 0. `go test ./...` is the no-native-dependency quick check and skips FFmpeg-tagged transcoding integration tests. The tagged build/test pair is the project baseline.

Verify workflow action majors remain Node 24-compatible: checkout/setup-go/upload-artifact v7, setup-buildx/login v4, build-push v7, golangci-lint v9, and action-gh-release v3. Do not add `ACTIONS_ALLOW_USE_UNSECURE_NODE_VERSION`.

## Verify Published Artifacts

Replace `vX.Y.Z` only with a tag whose workflow completed successfully.

```bash
export LIVEFORGE_VERSION=vX.Y.Z
gh run list --workflow Release --branch "$LIVEFORGE_VERSION"
gh release view "$LIVEFORGE_VERSION"
docker manifest inspect "ghcr.io/im-pingo/liveforge:$LIVEFORGE_VERSION" >/dev/null
```

These commands must exit 0 and show the expected tag/assets before the image or binaries are described as available. A missing release/image is expected before the first successful tagged workflow and must not be papered over with `latest`.

For an authenticated private GHCR package:

```bash
docker login ghcr.io
docker pull "ghcr.io/im-pingo/liveforge:$LIVEFORGE_VERSION"
```

Start only on loopback/private interfaces with changed credentials, TLS/auth enabled, and required ports restricted. Probe the public health endpoint and an authenticated endpoint:

```bash
curl -fsS http://127.0.0.1:8090/api/v1/server/health
curl -fsS -H "Authorization: Bearer $ADMIN_TOKEN" \
  http://127.0.0.1:8090/api/v1/server/info
```

Both return 200 when the deployment is ready. Authentication failures are 401, authorization failures 403, and enabled rate limiting can return 429.

## Diagnostics And Metrics

Inspect the GitHub Actions job logs, release asset list, OCI manifest platforms (`linux/amd64`, `linux/arm64`), and binary asset names (`linux`/`darwin`, `amd64`/`arm64`). After startup, verify `/metrics`, `liveforge_server_uptime_seconds`, module status endpoints, and one protocol smoke test appropriate to the deployment. The presence of the metrics endpoint does not prove audio transcoding is compiled in.

## Rollback And Recovery

1. Keep the previously verified version or commit SHA available.
2. On a bad release, deploy the previous immutable version; do not mutate or reuse the failed tag.
3. Publish a new patch version after fixes and rerun all gates.
4. If only audio transcoding is missing, replace the portable binary with a verified tagged source build or released Docker image; do not claim the portable release binary supports it.
5. If an image is private, either authenticate clients or intentionally change package visibility. Do not bypass registry verification with an unverified mirror.
