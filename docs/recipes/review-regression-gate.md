# Review Regression Gate

This gate exercises the September 2026 review fixes on an isolated test
instance. It does not contact a production deployment or delete live streams.

## Prerequisites

- Go 1.26+, CGO, and FFmpeg development libraries for the tagged media matrix.
- Chrome/Chromium with H.264 WebRTC receive support on the test host.
- Available loopback ports; do not run two cluster test binaries concurrently
  because some legacy fixtures reserve the same ports.

Set `CHROME_BIN` to the H.264-capable browser executable when multiple browsers
are installed. CI uses `/usr/bin/google-chrome` and prints its version before
testing; chromedp's automatic Linux search prefers the runner's Chromium
snapshot, which does not advertise H.264. All Console and tagged media browser
tests use this selection. An unset value preserves automatic discovery; an
invalid explicit path never falls back and fails in required-browser mode.
For example, on macOS:

```sh
CHROME_BIN='/Applications/Google Chrome.app/Contents/MacOS/Google Chrome' \
  tools/check-review-regressions.sh
```

Run `tools/check-review-regressions.sh`. The gate includes repeated concurrent
ingress, generation replacement, lifecycle backpressure, cluster admission and
cleanup, config conflict/history, registered-handler RBAC, body-timeout,
recording-index and three-RID Simulcast isolation/selection/pause regressions,
four two-worker five-second media fuzz runs, Console browser checks, and the
SIP/GB28181/WHIP browser matrix. Each browser matrix scenario checks decoded
dimensions, advancing media time, RTP/RTCP counters, and non-stalled WHEP state.
Set `LIVEFORGE_REVIEW_SOAK_SECONDS` to an integer from 1 to 300; default 60.
These bounded fixtures are regression evidence, not a deployment capacity
guarantee. Longer workload and real-network testing remain deployment tasks.

The WHIP test publisher spaces each converted Opus packet by its own duration,
rebasing after a late timer wakeup and preserving spacing when the video clock
has advanced ahead of buffered audio. Controlled-clock sender tests prove this
under oversleep; real Pion tests validate RTP timestamp durations after an
intentional receiver backlog. Receiver `ReadRTP` arrival times are not treated
as evidence of sender pacing because scheduler delays can batch those reads.

Shared HTTP muxer tests distinguish overwrite from clean completion. Terminal
notification precedes reader-pump cancellation, including externally injected
termination; the cancellation-boundary regression requires the overwrite cause
instead of EOF. fMP4 discards pending fragments on overwrite and retains clean
completion flushing.

`LIVEFORGE_REQUIRE_BROWSER=1` makes unavailable Chrome, unsupported H.264, or
short-mode browser skips fail the required Console/media checks. The CI tagged
suite enables this flag with a 15-second per-scenario matrix soak. Ordinary
local package tests retain explicit environment skips when the flag is absent.
CI also passes its just-built executable through `LF_BINARY`, enabling the real
origin-edge subprocess test. Use an absolute current binary path for equivalent
local full-suite coverage. The topology's looping publisher uses real-time
pacing; unpaced saturation can overwrite the bounded relay ring and correctly
terminate the generation before playback begins. Generated node configs retain
production GOP hard bounds, and discovery parses the management API envelope.

## Input Bounds

Management API and WebRTC HTTP listeners have a 10-second total request read
deadline, in addition to five-second header and two-minute keep-alive idle
timeouts. Streaming response lifetimes are not limited by this read deadline.
An incomplete WHIP/WHEP SDP body returns 408 and releases its connection slot;
SDP size remains limited to 1 MiB. Cluster signaling bodies are limited to
64 KiB and pending GB PS assembly to 4 MiB.

The shared H.264 SPS parser rejects malformed/truncated syntax, more than 255
POC reference-cycle entries, invalid chroma or cropping, and coded dimensions
beyond the global H.264 Level 6.2 bounds (139,264 macroblocks total and 1,055
macroblocks per dimension). fMP4 avcC inspection uses the same parser. H.265 FU
assembly retains at most 16 MiB and 16,384 fragments, validates RTP sequence,
timestamp, SSRC, payload type, and NAL header continuity, and clears incomplete
state on error. A subsequent fresh start can recover normally.

The shared VP8 depacketizer uses Pion's RFC 7741 descriptor parser, including
extended picture IDs. A frame must start at S=1/PID=0; continuation packets must
match sequence, timestamp, SSRC, and payload type. Frame storage length/capacity
is bounded to 16 MiB and 16384 fragments; malformed/discontinuous/oversized input
releases partial state. This applies to ordinary WHIP and Simulcast alike.

## Management Lists

Streams, recordings, SIP calls, and audit lists accept `limit` (1..500) and
`offset` (non-negative). Supplying only offset selects limit 100. Without
pagination parameters, legacy response shapes and complete lists remain
available. Paginated responses retain `data` and add top-level
`pagination: {limit, offset, total, has_more}` plus `X-Total-Count`, `X-Limit`, and
`X-Offset` headers. Filters run before slicing. Each request is a live snapshot;
offset pages can shift when records are added or removed.

Audit accepts exact `principal`, `action`, and `result`, inclusive RFC3339
`since`/`until`, and `order=asc|desc`. `format=ndjson` downloads matching retained
entries as an authenticated attachment. The export contains retained history,
not an unlimited archive.
