# WebRTC Pacer Catch-up Strategy Fix

## Problem

WebRTC (WHEP) playback stutters in a periodic cycle: ~2s freeze followed by ~1s playback. Video content during the freeze is skipped. The cycle period matches the GOP interval.

### Root Cause

In `whepFeedLoop` (`module/webrtc/whep.go`), the DTS-based pacer has a flawed catch-up strategy:

```
sleepDur > 0            → sleep (correct pacing)
-500ms < sleepDur ≤ 0   → send immediately, no pacing (BURST)
sleepDur ≤ -500ms       → reset pace base
```

When the feed loop falls slightly behind (e.g., after writing a large keyframe), all accumulated frames are sent in a burst with no inter-frame pacing. This causes the browser's jitter buffer to inflate, freeze, and then skip content to catch up. The cycle repeats every GOP because each keyframe triggers a new burst.

### Why SlowConsumerFilter Is Not Needed

WebRTC uses UDP (DTLS-SRTP), so `WriteSample` rarely blocks due to network backpressure. The DTS pacer naturally controls send rate. Once the pacer's catch-up strategy is fixed to drop non-keyframes instead of bursting, it fulfills the same role as `SlowConsumerFilter` does for TCP-based protocols (RTMP/SRT/RTSP). Adding both would create two independent drop mechanisms that could conflict.

## Design

### New Pacer Strategy

Replace the 500ms threshold with a 1-frame-interval threshold (40ms). When behind by more than one frame interval, enter catch-up mode: skip video interframes, keep keyframes/audio/sequence headers, and reset the pace base on the next keyframe.

```
sleepDur > 0                → sleep (normal pacing)
-catchUpThreshold < sleepDur ≤ 0  → send immediately (allow ≤1 frame jitter)
sleepDur ≤ -catchUpThreshold      → catch-up mode: skip video interframes
```

Where `catchUpThreshold = 40ms` (~1 frame at 25fps).

### State Transitions

```
                    sleepDur > -40ms
     +-------------------------------------+
     |                                     v
+--------+    sleepDur ≤ -40ms      +----------+
| Normal | -----------------------> | CatchUp  |
+--------+                         +----------+
     ^                                  |
     +----------------------------------+
         keyframe arrived → reset base
```

- **Normal**: all frames paced by DTS and sent.
- **CatchUp**: video interframes skipped. Keyframes, audio frames, and sequence headers are still sent. When a video keyframe arrives, reset `paceBaseWall` and `paceBaseDTS` to re-anchor timing, then transition back to Normal.

### Frame Delivery Rules in CatchUp Mode

| Frame Type | Delivered? | Reason |
|---|---|---|
| Video keyframe | Yes + reset pace base | Clean decoder resync point |
| Video interframe | No (skipped) | Cannot decode without preceding frames |
| Video sequence header | Yes | SPS/PPS needed for keyframe decoding |
| Audio | Yes | Independent of video, keeps audio continuous |

### DTS Tracking for Skipped Frames

When skipping a video interframe, `lastVideoDTS` is still updated to the skipped frame's DTS. This ensures the first keyframe after catch-up gets a normal duration (~40ms) instead of a multi-second gap that would corrupt Chrome's jitter buffer timing. This follows the same pattern already used for PLI/FIR resync in the existing code.

### Constants

```go
const (
    // catchUpThreshold is the maximum DTS lag (in wall-clock time) before
    // the pacer enters catch-up mode. Set to ~1 frame interval at 25fps.
    // Frames within this threshold are sent immediately (minor jitter OK).
    // Beyond this threshold, non-keyframes are skipped to prevent bursting.
    catchUpThreshold = 40 * time.Millisecond
)
```

## Affected Code

Only `module/webrtc/whep.go`, function `whepFeedLoop`. ~20 lines changed.

### Before (lines 457-476)

```go
dtsDelta := time.Duration(frame.DTS-paceBaseDTS) * time.Millisecond
targetTime := paceBaseWall.Add(dtsDelta)
sleepDur := time.Until(targetTime)

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
```

### After

```go
dtsDelta := time.Duration(frame.DTS-paceBaseDTS) * time.Millisecond
targetTime := paceBaseWall.Add(dtsDelta)
sleepDur := time.Until(targetTime)

if sleepDur > 0 && sleepDur < time.Second {
    // Ahead of schedule: pace to match real-time.
    timer := time.NewTimer(sleepDur)
    select {
    case <-timer.C:
    case <-done:
        timer.Stop()
        return
    }
} else if sleepDur < -catchUpThreshold {
    // Behind by more than 1 frame interval: catch-up mode.
    // Skip video interframes to avoid bursting the jitter buffer.
    // Audio and sequence headers pass through to keep audio continuous.
    if frame.MediaType.IsVideo() {
        if frame.FrameType == avframe.FrameTypeKeyframe {
            // Keyframe: reset pace base and deliver.
            paceBaseWall = time.Now()
            paceBaseDTS = frame.DTS
        } else if frame.FrameType != avframe.FrameTypeSequenceHeader {
            // Interframe: skip, but track DTS for correct duration on next keyframe.
            if frame.DTS > 0 {
                lastVideoDTS = frame.DTS
            }
            continue
        }
    }
    // Audio and sequence headers: deliver without pacing.
}
// sleepDur in [-catchUpThreshold, 0]: send immediately (minor jitter acceptable).
```

## Not in Scope

- SlowConsumerFilter integration for WebRTC (not needed; pacer handles this role)
- Simulcast-based quality switching
- Configurable catchUpThreshold (hardcoded 40ms is sufficient for 25-30fps streams)
- Changes to other protocols
