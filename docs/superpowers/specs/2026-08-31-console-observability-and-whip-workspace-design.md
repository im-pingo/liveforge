# Console Observability and WHIP Workspace Design

## Scope

Improve the embedded web console in two user-facing areas:

1. Keep the Streams list visually stable while live values change, especially
   GOP cache values.
2. Add an on-demand stream detail panel with a 60-second, one-sample-per-second
   trend view for bitrate, video/audio frame rate, and GOP duration.
3. Replace the WHIP publish modal flow with a dedicated `/console/publish`
   workspace that feels like a broadcaster or video-conference control surface.

The existing REST API remains the source of truth. No new media or signaling
protocol is introduced.

## Recommended Approach

Keep one embedded `module/api/console.html` and serve it for both `/console` and
`/console/publish`. A small route-aware bootstrap hides the management shell and
shows the publish shell when the latter path is loaded. This keeps WHIP device,
ICE, lifecycle, and stats code in one place while preserving the existing
console session authentication and static asset paths.

The header Publish action navigates to `/console/publish`; the publish shell
provides a clear return link to `/console`. The old publish overlay is removed
from the user flow.

## Streams Presentation

### Stable list

- Render the table with a fixed layout and explicit column tracks so changing
  values cannot resize neighboring columns.
- Reconcile rows by stream key. Existing `tr` nodes and their child value nodes
  are updated in place; rows for removed keys are deleted and new keys inserted
  in sorted order.
- Keep GOP values in fixed-width numeric slots with tabular numerals. Use
  separate labels for generation, frame count, duration, and V/A frame counts
  so text changes do not cause reflow.
- Make each stream row selectable with keyboard support. The selected row has a
  stable visual state and does not depend on a modal.

### Detail trend panel

- Place a detail panel directly below the Streams table. It is hidden until a
  row is selected and closes when the selected stream disappears.
- Poll `GET /api/v1/streams/{stream_key}` once per second while the Streams view
  is visible and a stream is selected. Keep the latest 60 samples in memory.
- Derive video FPS and audio FPS from cumulative `video_frames` and
  `audio_frames` deltas. Use the server-provided instantaneous bitrate and GOP
  duration for the other series.
- Provide a compact metric switcher for bitrate, video FPS, audio FPS, and GOP
  duration. Draw the active series with an inline SVG path and stable viewBox;
  show the latest value, min/max range, and a no-data state.
- Reset the sample window when the stream generation changes or the selected
  stream is replaced. Stop the detail poller when the view is hidden, the row is
  deselected, or the page is not visible.

The existing three-second management refresh remains the list refresh cadence;
the one-second detail request is scoped to the selected stream only.

## WHIP Publish Workspace

### Layout

- Use the existing dark console palette with a dedicated page header, product
  identity, live connection badge, and a return-to-console link.
- Desktop: two-column grid with a large mirrored local preview on the left and
  a control rail on the right.
- Control rail: stream key, camera, microphone, codec selection, start/stop
  action, and an explicit status/error region.
- Show a compact stats strip below the preview for outbound video/audio bitrate,
  encoded FPS, keyframes, RTT, jitter, and packet loss. Reuse the current
  `RTCPeerConnection#getStats` sampling and delta calculations.
- Mobile: collapse to one column with the controls below the preview; all
  controls remain reachable without horizontal scrolling.

### Lifecycle and failure behavior

- Entering `/console/publish` only prepares the page. Camera/microphone access
  is requested in the same user-visible flow as today, with clear secure-context
  and permission errors.
- Starting publish keeps the existing WHIP offer/answer and ICE behavior.
- Stopping publish closes the peer connection, deletes the WHIP session, stops
  local tracks, clears stats, and leaves the user on the publish page so they can
  start another stream or return to Console.
- A failed or disconnected connection restores the start state and leaves the
  error visible until the next attempt.

## Server and Routing

- Add `GET /console/publish` to the API route registration and serve the same
  embedded console document.
- Keep `/console` and `/console/publish` behind the existing console session
  authentication middleware.
- No API schema or WebRTC signaling contract changes are required.

## Testing and Documentation

- Update console DOM/script tests for keyed stream reconciliation, fixed GOP
  rendering, selection/detail sampling, and route-aware publish initialization.
- Update the browser publish test to navigate to `/console/publish` and verify
  the full-page flow and existing WHIP session behavior.
- Add a route test for `/console/publish` and retain `/console` compatibility.
- Run focused `module/api` tests, `go test ./...`, and
  `tools/check-agent-docs_test.sh`.
- Update `README.md`, `README.zh-CN.md`, `llms.txt`, and `llms-full.txt` to
  describe the stable Streams detail view and `/console/publish` workflow.
  `agent-manifest.json` does not need capability changes because the underlying
  WHIP capability and API contracts are unchanged.

## Non-goals

- No persistent time-series storage or server-side metrics history.
- No new charting dependency; the chart is rendered with browser primitives.
- No changes to stream statistics calculation or WHIP signaling semantics.
