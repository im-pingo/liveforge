package webrtc

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	"github.com/im-pingo/liveforge/pkg/audiocodec"
	"github.com/im-pingo/liveforge/pkg/avframe"
)

// TestWHEPBrowserJitterDiagnostic launches headless Chrome to play a WHEP
// stream and collects real browser WebRTC stats (getStats API) over 60 seconds.
//
// Uses VP8 video (universally decoded in headless Chrome without GPU) to get
// real decode and jitter buffer metrics — the same stats visible in
// chrome://webrtc-internals.
//
// It runs two scenarios: video-only and video+audio transcoding.
func TestWHEPBrowserJitterDiagnostic(t *testing.T) {
	// Set up Chrome allocator.
	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(),
		append(chromedp.DefaultExecAllocatorOptions[:],
			chromedp.Flag("headless", true),
			chromedp.Flag("disable-gpu", true),
			chromedp.Flag("no-sandbox", true),
			chromedp.Flag("autoplay-policy", "no-user-gesture-required"),
			chromedp.Flag("use-fake-device-for-media-stream", true),
		)...,
	)
	defer allocCancel()

	type scenario struct {
		name       string
		withAudio  bool
		streamPath string
	}
	scenarios := []scenario{
		{"video_only", false, "live/browser-jitter-vo"},
		{"video_audio_transcode", true, "live/browser-jitter-av"},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			runBrowserJitterDiagnostic(t, allocCtx, sc.withAudio, sc.streamPath)
		})
	}
}

func runBrowserJitterDiagnostic(t *testing.T, allocCtx context.Context, withAudio bool, streamPath string) {
	t.Helper()

	m, s := newTestModule(t)
	if withAudio {
		s.StreamHub().SetAudioCodecEnabled(true)
	}

	stream, err := s.StreamHub().GetOrCreate(streamPath)
	if err != nil {
		t.Fatal(err)
	}

	// Use VP8: Chrome headless decodes VP8 in software without GPU.
	pubInfo := &avframe.MediaInfo{VideoCodec: avframe.CodecVP8}
	if withAudio {
		pubInfo.AudioCodec = avframe.CodecAAC
		pubInfo.SampleRate = 48000
		pubInfo.Channels = 2
	}
	pub := &testPublisher{id: "browser-jitter-pub", info: pubInfo}
	if err := stream.SetPublisher(pub); err != nil {
		t.Fatal(err)
	}

	// Give the stream a moment to fully initialize before browser connects.
	time.Sleep(100 * time.Millisecond)

	// Load real VP8 frames from IVF fixture.
	vp8Frames := loadVP8TestFixture(t, "testdata/test_320x180.ivf")
	const durationSec = 60
	const fps = 25
	const frameDurMs = 40
	const totalFrames = durationSec * fps
	const gopInterval = 25
	expandedFrames := expandVP8Frames(vp8Frames, totalFrames, frameDurMs, gopInterval)

	// Write initial keyframe.
	stream.WriteFrame(&expandedFrames[0])

	// Pre-encode AAC frames if audio is needed.
	var aacPayloads [][]byte
	if withAudio {
		stream.WriteFrame(&avframe.AVFrame{
			MediaType: avframe.MediaTypeAudio,
			Codec:     avframe.CodecAAC,
			FrameType: avframe.FrameTypeSequenceHeader,
			Payload:   []byte{0x11, 0x90},
			DTS:       0,
		})
		aacEnc := audiocodec.NewFFmpegEncoder("aac", 48000, 2)
		defer aacEnc.Close()
		for range 3000 { // ~60s of audio at ~21ms per frame
			pcm := &audiocodec.PCMFrame{
				Samples:    make([]int16, 1024*2),
				SampleRate: 48000,
				Channels:   2,
			}
			data, err := aacEnc.Encode(pcm)
			if err != nil {
				t.Fatalf("AAC encode: %v", err)
			}
			aacPayloads = append(aacPayloads, data)
		}
	}

	// Get the WebRTC module's actual address for WHEP.
	whepAddr := m.Addr().String()
	// Replace wildcard bind addresses with localhost for browser access.
	if strings.HasPrefix(whepAddr, "[::]:") {
		whepAddr = "localhost:" + strings.TrimPrefix(whepAddr, "[::]:")
	} else if strings.HasPrefix(whepAddr, "0.0.0.0:") {
		whepAddr = "localhost:" + strings.TrimPrefix(whepAddr, "0.0.0.0:")
	}
	whepBase := "http://" + whepAddr

	// Serve a minimal player page.
	playerHTML := buildPlayerHTML(whepBase, streamPath)
	pageSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(playerHTML))
	}))
	defer pageSrv.Close()

	// Launch a fresh Chrome context for this scenario.
	browserCtx, browserCancel := chromedp.NewContext(allocCtx,
		chromedp.WithLogf(t.Logf),
	)
	defer browserCancel()

	// Listen for console messages.
	chromedp.ListenTarget(browserCtx, func(ev interface{}) {
		if ev, ok := ev.(*runtime.EventConsoleAPICalled); ok {
			var args []string
			for _, arg := range ev.Args {
				args = append(args, string(arg.Value))
			}
			t.Logf("[chrome console] %s", strings.Join(args, " "))
		}
	})

	// Navigate to player page (no timeout context, use browserCtx directly).
	if err := chromedp.Run(browserCtx,
		chromedp.Navigate(pageSrv.URL),
		chromedp.WaitVisible("#status", chromedp.ByID),
	); err != nil {
		t.Fatalf("Failed to navigate to player: %v", err)
	}

	t.Log("Browser navigated to player page, waiting for connection...")

	// Wait for connection with a simple sleep + poll approach.
	time.Sleep(5 * time.Second) // give ICE gathering + WHEP negotiation time

	var statusText string
	if err := chromedp.Run(browserCtx,
		chromedp.TextContent("#status", &statusText),
	); err != nil {
		t.Fatalf("Failed to get status: %v", err)
	}

	if statusText != "connected" && statusText != "completed" {
		t.Fatalf("WebRTC not connected after 5s. Status: %q", statusText)
	}

	t.Log("WebRTC connected, starting frame pump...")

	// Start pumping VP8 frames in background.
	const audioFrameDurMs = 21

	pumpDone := make(chan struct{})
	go func() {
		defer close(pumpDone)
		sendStart := time.Now()
		audioIdx := 0
		nextAudioDTS := int64(0)

		for i := 1; i < totalFrames; i++ { // frame 0 already written above
			dts := int64(i) * frameDurMs
			targetTime := sendStart.Add(time.Duration(dts) * time.Millisecond)
			if sleepDur := time.Until(targetTime); sleepDur > 0 {
				time.Sleep(sleepDur)
			}

			stream.WriteFrame(&expandedFrames[i])

			if withAudio {
				for nextAudioDTS <= dts+int64(frameDurMs) && audioIdx < len(aacPayloads) {
					stream.WriteFrame(&avframe.AVFrame{
						MediaType: avframe.MediaTypeAudio,
						Codec:     avframe.CodecAAC,
						FrameType: avframe.FrameTypeInterframe,
						Payload:   aacPayloads[audioIdx],
						DTS:       nextAudioDTS,
					})
					audioIdx++
					nextAudioDTS += int64(audioFrameDurMs)
				}
			}
		}
	}()

	// Collect stats from browser every second.
	var snapshots []statsSnapshot

	t.Log("Collecting browser stats for", durationSec, "seconds...")

	for sec := 1; sec <= durationSec; sec++ {
		time.Sleep(1 * time.Second)

		var result string
		evalCtx, evalCancel := context.WithTimeout(browserCtx, 3*time.Second)
		err := chromedp.Run(evalCtx,
			chromedp.Evaluate(`window.__getStatsJSON ? window.__getStatsJSON() : '{}'`, &result),
		)
		evalCancel()
		if err != nil {
			t.Logf("[%2ds] Failed to get stats: %v", sec, err)
			continue
		}

		snap := parseStatsJSON(result, sec)
		if snap != nil {
			if len(snapshots) > 0 {
				prev := snapshots[len(snapshots)-1]
				snap.PacketsLostDelta = snap.PacketsLost - prev.PacketsLost
				snap.FramesDroppedDelta = snap.FramesDropped - prev.FramesDropped
			}
			snapshots = append(snapshots, *snap)

			// Log significant events.
			if snap.FreezeCount > 0 && (len(snapshots) < 2 || snap.FreezeCount > snapshots[len(snapshots)-2].FreezeCount) {
				t.Logf("[%2ds] FREEZE! count=%d total=%.0fms", sec, snap.FreezeCount, snap.FreezeDurationMs)
			}
			if snap.PacketsLostDelta > 0 {
				t.Logf("[%2ds] PACKET LOSS: +%d (cum=%d)", sec, snap.PacketsLostDelta, snap.PacketsLost)
			}
			if snap.JitterBufferMs > 200 {
				t.Logf("[%2ds] JITTER BUFFER HIGH: %.0fms", sec, snap.JitterBufferMs)
			}
		}
	}

	<-pumpDone

	// === Analysis ===
	if len(snapshots) == 0 {
		t.Fatal("No stats collected from browser")
	}

	fmt.Printf("\n=== BROWSER JITTER DIAGNOSTIC [%s] ===\n", t.Name())
	fmt.Println("sec | jitbuf(ms) | jitter(ms) |  fps  | lost(Δ) | freeze | drop(Δ) | nack | pli")
	fmt.Println("----|------------|------------|-------|---------|--------|---------|------|----")

	var maxJB, maxJitter float64
	var totalLostDelta, totalDropDelta int
	var lastFreeze int
	var minFPS float64 = 999

	for _, s := range snapshots {
		fmt.Printf("%3d | %10.1f | %10.1f | %5.1f | %7d | %6d | %7d | %4d | %3d\n",
			s.Second, s.JitterBufferMs, s.JitterRTPMs, s.FPS,
			s.PacketsLostDelta, s.FreezeCount, s.FramesDroppedDelta,
			s.NACKCount, s.PLICount)

		if s.JitterBufferMs > maxJB {
			maxJB = s.JitterBufferMs
		}
		if s.JitterRTPMs > maxJitter {
			maxJitter = s.JitterRTPMs
		}
		if s.FPS > 0 && s.FPS < minFPS {
			minFPS = s.FPS
		}
		totalLostDelta += s.PacketsLostDelta
		totalDropDelta += s.FramesDroppedDelta
		lastFreeze = s.FreezeCount
	}

	fmt.Println()
	fmt.Printf("Peak jitter buffer: %.1f ms\n", maxJB)
	fmt.Printf("Peak RTP jitter:    %.1f ms\n", maxJitter)
	fmt.Printf("Min FPS:            %.1f\n", minFPS)
	fmt.Printf("Total lost:         %d\n", totalLostDelta)
	fmt.Printf("Total drops:        %d\n", totalDropDelta)
	fmt.Printf("Freeze count:       %d\n", lastFreeze)

	// === Assertions ===
	if lastFreeze > 2 {
		t.Errorf("Browser reported %d freeze events — video is stuttering", lastFreeze)
	}
	if maxJB > 500 {
		t.Errorf("Peak jitter buffer %.0fms exceeds 500ms — indicates delivery timing issues", maxJB)
	}
	if totalLostDelta > 20 {
		t.Errorf("Total packet loss %d exceeds threshold — indicates network or pacing issues", totalLostDelta)
	}
}

type statsSnapshot struct {
	Second             int
	JitterBufferMs     float64
	JitterRTPMs        float64
	FPS                float64
	PacketsLost        int
	PacketsLostDelta   int
	FreezeCount        int
	FreezeDurationMs   float64
	FramesDropped      int
	FramesDroppedDelta int
	NACKCount          int
	PLICount           int
	PacketsReceived    int
}

// parseStatsJSON parses the JSON returned by window.__getStatsJSON().
func parseStatsJSON(jsonStr string, sec int) *statsSnapshot {
	if jsonStr == "{}" || jsonStr == "" {
		return nil
	}

	get := func(key string) float64 {
		idx := strings.Index(jsonStr, `"`+key+`"`)
		if idx < 0 {
			return 0
		}
		rest := jsonStr[idx+len(key)+3:] // skip `"key":`
		end := strings.IndexAny(rest, ",}")
		if end < 0 {
			return 0
		}
		val := strings.TrimSpace(rest[:end])
		var f float64
		fmt.Sscanf(val, "%f", &f)
		return f
	}

	return &statsSnapshot{
		Second:           sec,
		JitterBufferMs:   get("jitterBufferMs"),
		JitterRTPMs:      get("jitterRTPMs"),
		FPS:              get("fps"),
		PacketsLost:      int(get("packetsLost")),
		FreezeCount:      int(get("freezeCount")),
		FreezeDurationMs: get("freezeDurationMs"),
		FramesDropped:    int(get("framesDropped")),
		NACKCount:        int(get("nackCount")),
		PLICount:         int(get("pliCount")),
		PacketsReceived:  int(get("packetsReceived")),
	}
}

// buildPlayerHTML creates a minimal WHEP player page that auto-connects
// and exposes getStats() results via window.__getStatsJSON().
func buildPlayerHTML(whepBase, streamPath string) string {
	return `<!DOCTYPE html>
<html><head><meta charset="utf-8"><title>Test Player</title></head>
<body>
<video id="video" autoplay muted playsinline style="width:320px;height:180px"></video>
<div id="status">Connecting...</div>
<input id="connected-flag" type="hidden" disabled>

<script>
let pc = null;
let prevVideo = null;
let prevTimestamp = 0;
let latestStats = {};

async function connect() {
  const whepUrl = '` + whepBase + `/webrtc/whep/` + streamPath + `?mode=realtime';
  console.log('Connecting to WHEP URL: ' + whepUrl);

  // No STUN needed — localhost test.
  pc = new RTCPeerConnection();

  pc.ontrack = (ev) => {
    console.log('ontrack: ' + ev.track.kind);
    if (ev.streams[0]) {
      document.getElementById('video').srcObject = ev.streams[0];
    }
  };

  pc.oniceconnectionstatechange = () => {
    const state = pc.iceConnectionState;
    console.log('ICE state: ' + state);
    document.getElementById('status').textContent = state;
    if (state === 'connected' || state === 'completed') {
      document.getElementById('connected-flag').disabled = false;
      startStats();
    }
  };

  pc.onicecandidate = (ev) => {
    if (ev.candidate) {
      console.log('ICE candidate: ' + ev.candidate.candidate);
    } else {
      console.log('ICE gathering complete');
    }
  };

  pc.addTransceiver('video', { direction: 'recvonly' });
  pc.addTransceiver('audio', { direction: 'recvonly' });

  try {
    const offer = await pc.createOffer();
    await pc.setLocalDescription(offer);
    console.log('Offer created, gathering ICE candidates...');

    // Wait for ICE gathering to complete (or timeout after 2s).
    await new Promise(resolve => {
      if (pc.iceGatheringState === 'complete') { resolve(); return; }
      const timer = setTimeout(() => {
        console.log('ICE gathering timeout, proceeding with gathered candidates');
        resolve();
      }, 2000);
      pc.onicegatheringstatechange = () => {
        if (pc.iceGatheringState === 'complete') {
          clearTimeout(timer);
          resolve();
        }
      };
    });

    console.log('Sending offer to ' + whepUrl);
    const resp = await fetch(whepUrl, {
      method: 'POST',
      headers: { 'Content-Type': 'application/sdp' },
      body: pc.localDescription.sdp,
    });

    console.log('Fetch response status: ' + resp.status);
    if (!resp.ok) {
      const errText = await resp.text();
      console.log('WHEP error: ' + resp.status + ' ' + errText);
      document.getElementById('status').textContent = 'Error: ' + resp.status;
      return;
    }

    const answerSdp = await resp.text();
    console.log('Got answer SDP (' + answerSdp.length + ' bytes)');
    await pc.setRemoteDescription({ type: 'answer', sdp: answerSdp });
    console.log('Remote description set');
  } catch (err) {
    console.log('Error: ' + err.message);
    console.log('Error stack: ' + err.stack);
    document.getElementById('status').textContent = 'Error: ' + err.message;
  }
}

function startStats() {
  setInterval(async () => {
    if (!pc) return;
    const stats = await pc.getStats();
    let video = null, audio = null;
    stats.forEach(r => {
      if (r.type === 'inbound-rtp' && r.kind === 'video') video = r;
      if (r.type === 'inbound-rtp' && r.kind === 'audio') audio = r;
    });

    const now = performance.now();
    if (video) {
      const dt = prevVideo ? (now - prevTimestamp) / 1000 : 1;

      // Jitter buffer average delay.
      const jbDelay = video.jitterBufferDelay || 0;
      const jbEmitted = video.jitterBufferEmittedCount || 0;
      const prevJbDelay = prevVideo ? (prevVideo.jitterBufferDelay || 0) : 0;
      const prevJbEmitted = prevVideo ? (prevVideo.jitterBufferEmittedCount || 0) : 0;
      const jbDelta = jbEmitted - prevJbEmitted;
      const jbAvgMs = jbDelta > 0 ? ((jbDelay - prevJbDelay) / jbDelta * 1000) : 0;

      const framesDecodedD = prevVideo ? (video.framesDecoded || 0) - (prevVideo.framesDecoded || 0) : 0;
      const fps = dt > 0 ? framesDecodedD / dt : 0;

      latestStats = {
        jitterBufferMs: Math.round(jbAvgMs * 10) / 10,
        jitterRTPMs: Math.round((video.jitter || 0) * 1000 * 10) / 10,
        fps: Math.round(fps * 10) / 10,
        packetsLost: video.packetsLost || 0,
        packetsReceived: video.packetsReceived || 0,
        freezeCount: video.freezeCount || 0,
        freezeDurationMs: Math.round((video.totalFreezesDuration || 0) * 1000),
        framesDropped: video.framesDropped || 0,
        nackCount: video.nackCount || 0,
        pliCount: video.pliCount || 0,
      };

      prevVideo = { ...video };
      prevTimestamp = now;
    }
  }, 500); // Collect at 2Hz for freshness, but test reads at 1Hz.
}

window.__getStatsJSON = function() {
  return JSON.stringify(latestStats);
};

connect();
</script>
</body>
</html>`
}

