# GCC Congestion Control + Pacer for WebRTC WHEP

## Problem

WHEP playback suffers from periodic stuttering (freeze-skip cycles) when the DTS-based pacer in `whepFeedLoop` falls behind. The current code has a 0–500ms "burst window" where accumulated frames are sent with no inter-frame pacing, causing the browser's jitter buffer to inflate and freeze.

The root cause is that pion's `WriteSample` sends RTP packets immediately through the interceptor chain with no send-side pacing. The application-layer DTS sleep is a workaround that cannot handle burst scenarios gracefully.

### Why Application-Layer Fixes Are Insufficient

The previous design (`2026-03-28-webrtc-pacer-catchup-design.md`) proposed catching up by dropping video interframes at a 40ms threshold. This is too aggressive for normal server-side jitter (GC, goroutine scheduling, large keyframe serialization) and would cause unnecessary quality degradation. Increasing the threshold to 150ms helps but is still a heuristic that cannot adapt to actual network conditions.

## Solution

Enable pion's built-in GCC (Google Congestion Control) `SendSideBWE` with `LeakyBucketPacer`. This moves burst smoothing from the application layer to the RTP interceptor layer, where it belongs.

## Architecture

### Three-Layer Responsibility Split

| Layer | Component | Responsibility |
|-------|-----------|---------------|
| Frame delivery | `whep_feed.go` DTS sleep | Deliver frames at real-time pace; never faster than wall clock |
| Congestion control | GCC `SendSideBWE` | Estimate available bandwidth from TWCC feedback |
| Packet pacing | `LeakyBucketPacer` | Release RTP packets at targetBitrate, 5ms tick interval |

### Data Flow

```
whepFeedLoop
  → DTS sleep (simplified: only sleep when ahead of real-time)
  → TrackSender.WriteSample
    → TrackLocalStaticSample.WriteSample
      → pion RTP packetizer
        → GCC cc.Interceptor.BindLocalStream (records send time, adds TWCC ext)
          → LeakyBucketPacer queue
            → 5ms ticker drains queue at targetBitrate budget → UDP

Browser → TWCC feedback RTCP
  → cc.Interceptor.BindRTCPReader
    → SendSideBWE.WriteRTCP
      → delayController + lossController estimate bandwidth
        → pacer.SetTargetBitrate(newRate)
```

### GCC Lifecycle

GCC `SendSideBWE` is **per-PeerConnection**, created automatically by pion's `cc.InterceptorFactory` when `NewPeerConnection` is called.

```
Module.Init
  → create cc.InterceptorFactory with GCC options
  → ir.Add(ccFactory)
  → ccFactory.OnNewPeerConnection(callback stores estimator)

handleWHEP
  → api.NewPeerConnection  // triggers callback synchronously
  → retrieve BandwidthEstimator from callback channel
  → pass to whepFeedLoop for monitoring/logging
```

## Config

New `GCCConfig` struct under `WebRTCConfig`:

```yaml
webrtc:
  gcc:
    enabled: true
    initial_bitrate: 2000000   # 2Mbps initial send rate
    min_bitrate: 100000        # 100kbps floor (extreme weak network)
    max_bitrate: 10000000      # 10Mbps ceiling (4K streams)
```

```go
type GCCConfig struct {
    Enabled        bool `yaml:"enabled"`
    InitialBitrate int  `yaml:"initial_bitrate"` // bits/sec, default 2_000_000
    MinBitrate     int  `yaml:"min_bitrate"`     // bits/sec, default 100_000
    MaxBitrate     int  `yaml:"max_bitrate"`     // bits/sec, default 10_000_000
}
```

**Parameter rationale:**
- **InitialBitrate 2Mbps**: covers most live streaming scenarios; GCC converges to actual bandwidth within seconds
- **MinBitrate 100kbps**: floor below which audio+video cannot decode meaningfully
- **MaxBitrate 10Mbps**: reasonable ceiling for 4K live streaming

When `gcc.enabled` is false, the interceptor is not registered and behavior is identical to the current codebase.

## Module.Init Changes

After `RegisterDefaultInterceptors` (which registers NACK + TWCC sender + Stats):

```go
if cfg.WebRTC.GCC.Enabled {
    bweFactory, err := cc.NewInterceptor(func() (cc.BandwidthEstimator, error) {
        return gcc.NewSendSideBWE(
            gcc.SendSideBWEInitialBitrate(cfg.WebRTC.GCC.InitialBitrate),
            gcc.SendSideBWEMinBitrate(cfg.WebRTC.GCC.MinBitrate),
            gcc.SendSideBWEMaxBitrate(cfg.WebRTC.GCC.MaxBitrate),
        )
    })
    if err != nil {
        return fmt.Errorf("webrtc: create GCC interceptor: %w", err)
    }
    m.latestBWE = make(chan cc.BandwidthEstimator, 1)
    bweFactory.OnNewPeerConnection(func(id string, estimator cc.BandwidthEstimator) {
        select {
        case m.latestBWE <- estimator:
        default:
        }
    })
    ir.Add(bweFactory)
}
```

### BWE Instance Delivery

The `OnNewPeerConnection` callback fires synchronously inside `NewPeerConnection`. A buffered channel (`cap=1`) captures the latest estimator. The WHEP handler reads it immediately after `NewPeerConnection` returns:

```go
// In handleWHEP, after NewPeerConnection:
var bwe cc.BandwidthEstimator
if m.latestBWE != nil {
    select {
    case bwe = <-m.latestBWE:
    default:
    }
}
```

WHIP does not need BWE (receive-only; no send-side pacing needed).

## whep_feed.go Changes

### Simplified DTS Pacing

Remove the catch-up and 500ms reset logic. Keep only the "don't outrun real-time" sleep:

```go
// Current code (lines 246-258):
if sleepDur > 0 && sleepDur < time.Second {
    timer := time.NewTimer(sleepDur)
    select {
    case <-timer.C:
    case <-done:
        timer.Stop()
        return
    }
} else if sleepDur < -500*time.Millisecond {
    paceBaseWall = time.Now()
    paceBaseDTS = frame.DTS
}

// New code:
if sleepDur > 0 && sleepDur < time.Second {
    // Ahead of real-time: sleep to match DTS pace.
    timer := time.NewTimer(sleepDur)
    select {
    case <-timer.C:
    case <-done:
        timer.Stop()
        return
    }
}
// sleepDur <= 0: behind real-time, deliver immediately.
// Pacer smooths the RTP output; no application-layer dropping needed.

// DTS discontinuity (stream restart, timestamp jump): reset base.
if sleepDur >= time.Second || sleepDur < -time.Second {
    paceBaseWall = time.Now()
    paceBaseDTS = frame.DTS
}
```

**What changes:**
- `sleepDur > 0`: sleep (unchanged)
- `sleepDur ∈ [-1s, 0]`: deliver immediately to WriteSample → pacer queue. No burst to network.
- `|sleepDur| > 1s`: DTS discontinuity, reset pace base
- **Removed**: 500ms threshold, catch-up mode, application-layer frame dropping

### Why Application-Layer Dropping Is No Longer Needed

Previously, burst-sending caused jitter buffer inflation. With the pacer:
1. Behind frames enter pacer queue rapidly
2. Pacer releases them at 5ms ticks within bitrate budget
3. Browser receives smooth RTP stream regardless of feed loop timing
4. If congestion occurs, GCC lowers targetBitrate → pacer slows → queue grows (bounded by queueSize=1M)

### GOP Cache Pacing

The existing 10x GOP cache pacing (lines 162-194) is **unchanged**. It operates before the live loop and serves a different purpose (immediate keyframe delivery at connection start).

### BWE Monitoring

Optional logging via `OnTargetBitrateChange`:

```go
if bwe != nil {
    bwe.OnTargetBitrateChange(func(bitrate int) {
        slog.Debug("GCC target bitrate changed",
            "module", "webrtc",
            "bitrate_kbps", bitrate/1000,
        )
    })
}
```

## Affected Files

| File | Change Type | Description |
|------|-------------|-------------|
| `config/config.go` | Modify | Add `GCCConfig` struct, add `GCC` field to `WebRTCConfig` |
| `config/loader.go` | Modify | Add GCC defaults (enabled=true, initial=2M, min=100K, max=10M) |
| `configs/liveforge.yaml` | Modify | Add `gcc` config section under `webrtc` |
| `module/webrtc/module.go` | Modify | Register GCC interceptor, add `latestBWE` channel, `cc`/`gcc` imports |
| `module/webrtc/whep.go` | Modify | Retrieve BWE after NewPeerConnection, pass to whepFeedLoop |
| `module/webrtc/whep_feed.go` | Modify | Simplify DTS pacing, add BWE param for logging, remove catch-up logic |
| `module/webrtc/whep_feed_test.go` | New | Test simplified pacer behavior |
| `module/webrtc/module_test.go` | Modify | Test GCC interceptor registration |

### Unchanged Files

| File | Reason |
|------|--------|
| `whip.go` | Receive-only, no send-side pacing needed |
| `track_sender.go` | WriteSample interface unchanged; pacer operates transparently at interceptor layer |
| `session.go` | No changes needed |
| All other modules | GCC is WebRTC-only |

## Invariants

- WHIP publish is unaffected
- GOP cache 10x pacing preserved
- PLI/FIR mechanism unchanged (TrackSender's needsKeyframe flag)
- `gcc.enabled: false` produces identical behavior to current code
- RegisterDefaultInterceptors still provides NACK, TWCC sender, Stats

## Interaction with Existing RTCP Feedback

| RTCP Type | Current Handler | GCC Interaction |
|-----------|----------------|-----------------|
| TWCC | Registered by `RegisterDefaultInterceptors` (sender extension) | GCC reads TWCC feedback via `BindRTCPReader` to estimate bandwidth |
| NACK | pion interceptor auto-retransmit | No conflict; retransmitted packets also go through pacer |
| PLI/FIR | TrackSender.rtcpLoop sets needsKeyframe flag | No conflict; PLI handling is independent of pacing |
| REMB | TrackSender.onREMB callback (monitoring only) | GCC does not use REMB; it uses TWCC exclusively |

## Testing Strategy

1. **Config tests**: verify GCCConfig defaults and YAML parsing
2. **Module init test**: verify GCC interceptor is registered when enabled, not registered when disabled
3. **Feed loop test**: verify simplified DTS logic — no frame dropping, pace base reset on discontinuity
4. **Integration test**: publish RTMP → subscribe WHEP → verify smooth playback without freeze-skip cycles

## Not in Scope

- Simulcast layer selection based on BWE (future work)
- WHIP receive-side bandwidth estimation
- Configurable pacer interval (pion default 5ms is appropriate)
- Per-stream GCC toggle (global config only)
