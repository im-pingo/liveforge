# P0/P1 Hardening and Browser Playback Design

## Context

`origin/fix/p0-p1-hardening` diverged from the current `main` at `e7a3dc3`, so replaying the branch as a merge would remove newer DVR, cluster, playback, and shutdown work. The implementation must preserve `origin/main` (`96976dc`) and port the branch behavior at the current module boundaries.

The current console builds media URLs from the configured HTTP listen value. `GET /api/v1/server/info` returns `:8080` even when the listener was allocated on a different port (especially in tests), and it does not expose a runtime endpoint identity. On the reported machine, the browser's loopback request reaches another process on port 8080 while WHEP uses 8443, explaining why WHEP works and every HTTP preview fails. The fix must make endpoint discovery use the bound listener address and surface HTTP/status failures before a player reports a misleading stall.

## Goals

1. Preserve all current `main` playback, DVR, cluster, recording, RTSP, and WebRTC behavior.
2. Port P0/P1 behavior: cancellation-safe ring readers, deterministic shutdown, immutable runtime settings, unified authorization, canonical path ownership, and terminal WebRTC cleanup.
3. Make console playback endpoint discovery accurate for bound ports and provide actionable media endpoint errors.
4. Add focused regressions for endpoint discovery, HTTP media responses, authorization, reader cancellation, and WebRTC cleanup.

## Architecture

### Endpoint discovery and preview

Introduce a small optional `core.EndpointProvider` interface implemented by listener-backed modules. The API handler reports `listener.Addr().String()` when the module is initialized and falls back to configured values for uninitialized/test-only handlers. The console normalizes wildcard addresses to the page host, preserves IPv6 brackets, and checks the media response status/content type before attaching a player. The API contract remains the same (`endpoints` is a map of protocol to `host:port`), but values now describe the active listener.

The HTTP streaming module remains the owner of FLV/TS/fMP4/HLS/DASH routes. No duplicate muxer or proxy path is introduced. The console's media error text includes the HTTP status and response content type, allowing a wrong-port/proxy response to be diagnosed immediately.

### Authorization and runtime snapshots

Add `core.Authorizer` as the common synchronous admission hook for publish and subscribe requests. Protocol modules construct one `EventContext` at admission, invoke authorization before allocating stream/session state, then emit lifecycle events only after the resource exists. Runtime configuration is published as an immutable snapshot; request paths read the snapshot or module-owned policy copies and never retain mutable configuration pointers.

### Lifecycle ownership

`StreamHub`, `Stream`, `MuxerManager`, and protocol sessions own their state transitions. Stream keys and route segments are canonicalized once at the boundary. Terminal WebRTC paths serialize cleanup, remove sessions/subscribers/connections exactly once, and close the underlying stream when a WHIP publisher reaches a terminal state. Ring readers use context-aware condition waits and unregister wake callbacks on close so shutdown cannot leave blocked readers or callbacks behind.

## Error handling

- A denied admission returns HTTP 401/403 or the protocol's existing rejection response and creates no stream, session, subscriber, or muxer.
- A media endpoint that is unreachable, non-2xx, or returns a non-media response is reported as a connection error in the console; the player is not left in a false "waiting" state.
- Runtime source failures retain the last valid immutable snapshot and expose status through the existing management API.
- Shutdown and cleanup are idempotent; repeated terminal callbacks are ignored after ownership is released.

## Verification

- Focused Go tests for `core`, `module/httpstream`, `module/api`, and `module/webrtc`.
- Browser/console tests assert the generated endpoint URL, reject wrong-port HTML/404 responses, and confirm decoded H.264 playback when a real HTTP listener is reachable.
- Run `tools/check-agent-docs_test.sh`, `go test ./...`, and the tagged race suite when FFmpeg development libraries are available.
- Synchronize `agent-manifest.json`, `llms-full.txt`, `llms.txt`, README files, and any affected recipe/API contract with the final behavior.
