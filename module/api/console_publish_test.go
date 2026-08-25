package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	webrtcmod "github.com/im-pingo/liveforge/module/webrtc"
	"github.com/im-pingo/liveforge/pkg/avframe"
)

// TestConsolePublishFlow launches headless Chrome on the console page
// (served over localhost HTTP — a secure context) and exercises the full
// WebRTC Publish workflow:
//  1. Open the publish modal
//  2. Verify fake camera/mic devices are enumerated
//  3. Enter a stream key and click "Start Publishing"
//  4. Verify the WHIP session succeeds and the stream appears in the hub
func TestConsolePublishFlow(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping browser test in short mode")
	}

	// --- start server with API + WebRTC modules ---
	cfg := &config.Config{
		API: config.APIConfig{
			Enabled: true,
			Listen:  "127.0.0.1:0",
			TLS:     boolPtr(false), // plain HTTP on localhost = secure context
		},
		WebRTC: config.WebRTCConfig{
			Enabled:      true,
			Listen:       "127.0.0.1:0",
			TLS:          boolPtr(false),
			UDPPortRange: []int{30000, 30100},
		},
		Stream: config.StreamConfig{
			GOPCache:           true,
			GOPCacheNum:        1,
			AudioCacheMs:       1000,
			RingBufferSize:     256,
			IdleTimeout:        30 * time.Second,
			NoPublisherTimeout: 30 * time.Second,
		},
	}

	srv := core.NewServer(cfg)
	apiMod := NewModule()
	webrtcMod := webrtcmod.NewModule()
	srv.RegisterModule(apiMod)
	srv.RegisterModule(webrtcMod)
	if err := srv.Init(); err != nil {
		t.Fatalf("server init: %v", err)
	}
	t.Cleanup(srv.Shutdown)

	apiAddr := apiMod.Addr().String()
	consoleURL := "http://" + apiAddr + "/console"

	// Patch the WebRTC listen address into the config so /api/v1/server/info
	// returns the actual port (config stores the original ":0").
	webrtcAddr := webrtcMod.Addr().String()
	cfg.WebRTC.Listen = webrtcAddr

	t.Logf("API:    %s", apiAddr)
	t.Logf("WebRTC: %s", webrtcAddr)
	t.Logf("Console: %s", consoleURL)

	// --- set up Chrome ---
	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(),
		append(chromedp.DefaultExecAllocatorOptions[:],
			chromedp.Flag("headless", true),
			chromedp.Flag("disable-gpu", true),
			chromedp.Flag("no-sandbox", true),
			// /dev/shm is tiny in CI containers; without this Chrome
			// crashes or hangs during startup.
			chromedp.Flag("disable-dev-shm-usage", true),
			chromedp.Flag("autoplay-policy", "no-user-gesture-required"),
			chromedp.Flag("use-fake-device-for-media-stream", true),
			chromedp.Flag("use-fake-ui-for-media-stream", true),
		)...,
	)
	defer allocCancel()

	browserCtx, browserCancel := chromedp.NewContext(allocCtx, chromedp.WithLogf(t.Logf))
	defer browserCancel()

	// Log Chrome console messages.
	chromedp.ListenTarget(browserCtx, func(ev interface{}) {
		if msg, ok := ev.(*runtime.EventConsoleAPICalled); ok {
			var args []string
			for _, arg := range msg.Args {
				args = append(args, string(arg.Value))
			}
			t.Logf("[chrome] %s", strings.Join(args, " "))
		}
	})

	// --- navigate to console ---
	t.Log("Navigating to console...")
	if err := chromedp.Run(browserCtx,
		chromedp.Navigate(consoleURL),
		chromedp.WaitReady("body"),
	); err != nil {
		// Browser startup failures (no Chrome binary, devtools websocket
		// timeout) are environment problems, not product bugs — skip so CI
		// hosts without a working Chrome don't fail the suite.
		if strings.Contains(err.Error(), "websocket url timeout") ||
			strings.Contains(err.Error(), "executable file not found") {
			t.Skipf("headless Chrome unavailable in this environment: %v", err)
		}
		t.Fatalf("navigate: %v", err)
	}

	// Wait for server info to load (endpoints populated).
	time.Sleep(2 * time.Second)

	// --- open publish modal ---
	t.Log("Opening publish modal...")
	if err := chromedp.Run(browserCtx,
		chromedp.Click("#btn-publish", chromedp.ByID),
	); err != nil {
		t.Fatalf("click publish button: %v", err)
	}

	// Wait for getUserMedia + enumerateDevices.
	time.Sleep(3 * time.Second)

	// --- verify devices enumerated ---
	var videoOptions int
	if err := chromedp.Run(browserCtx,
		chromedp.Evaluate(`document.getElementById("publish-video-device").options.length`, &videoOptions),
	); err != nil {
		t.Fatalf("get video options: %v", err)
	}
	t.Logf("Video device options: %d", videoOptions)
	if videoOptions < 1 {
		t.Fatal("No video devices enumerated — fake device flag may not be working")
	}

	var audioOptions int
	if err := chromedp.Run(browserCtx,
		chromedp.Evaluate(`document.getElementById("publish-audio-device").options.length`, &audioOptions),
	); err != nil {
		t.Fatalf("get audio options: %v", err)
	}
	t.Logf("Audio device options: %d", audioOptions)
	if audioOptions < 1 {
		t.Fatal("No audio devices enumerated")
	}

	// Check publish status — should be empty (no error).
	var publishStatus string
	if err := chromedp.Run(browserCtx,
		chromedp.TextContent("#publish-status", &publishStatus),
	); err != nil {
		t.Fatalf("get publish status: %v", err)
	}
	if publishStatus != "" {
		t.Fatalf("Expected empty publish status, got: %q", publishStatus)
	}

	// --- set stream key and start publishing ---
	streamKey := "live/browser-publish-test"
	t.Logf("Publishing to %s ...", streamKey)

	if err := chromedp.Run(browserCtx,
		chromedp.Clear("#publish-stream-key"),
		chromedp.SendKeys("#publish-stream-key", streamKey),
		chromedp.Click("#btn-start-publish", chromedp.ByID),
	); err != nil {
		t.Fatalf("start publish: %v", err)
	}

	// Wait for WHIP negotiation + ICE connection.
	var status string
	connected := false
	for i := 0; i < 15; i++ {
		time.Sleep(1 * time.Second)

		// Log ICE state from browser side.
		var iceState string
		chromedp.Run(browserCtx,
			chromedp.Evaluate(`publishPC ? publishPC.iceConnectionState : "no-pc"`, &iceState),
		)

		if err := chromedp.Run(browserCtx,
			chromedp.TextContent("#publish-status", &status),
		); err != nil {
			t.Logf("[%ds] status read error: %v", i+1, err)
			continue
		}
		t.Logf("[%ds] publish status: %q, ICE: %s", i+1, status, iceState)

		if strings.Contains(status, "Publishing") {
			connected = true
			break
		}
		if strings.Contains(status, "error") || strings.Contains(status, "Error") {
			t.Fatalf("Publish failed: %s", status)
		}
	}

	if !connected {
		t.Fatalf("Publish did not reach 'Publishing' state after 15s. Last status: %q", status)
	}

	// --- verify stream exists in the hub ---
	keys := srv.StreamHub().Keys()
	found := false
	for _, k := range keys {
		if k == streamKey {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Stream %q not found in hub. Hub keys: %v", streamKey, keys)
	}

	// Verify the stream has a publisher.
	stream, ok := srv.StreamHub().Find(streamKey)
	if !ok {
		t.Fatalf("Stream %q not found after Keys() returned it", streamKey)
	}
	if stream.Publisher() == nil {
		t.Error("Stream has no publisher — WHIP session did not register")
	} else {
		t.Logf("Publisher ID: %s", stream.Publisher().ID())
		t.Logf("MediaInfo: %+v", stream.Publisher().MediaInfo())
	}

	t.Log("Console publish test PASSED")
}

func TestConsoleWHEPWaitsForDecodedFrameBeforeReportingPlaying(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping browser test in short mode")
	}

	cfg := &config.Config{
		API: config.APIConfig{Enabled: true, Listen: "127.0.0.1:0", TLS: boolPtr(false)},
		WebRTC: config.WebRTCConfig{
			Enabled:      true,
			Listen:       "127.0.0.1:0",
			TLS:          boolPtr(false),
			UDPPortRange: []int{30101, 30200},
		},
		Stream: config.StreamConfig{
			GOPCache:           true,
			GOPCacheNum:        1,
			AudioCacheMs:       1000,
			RingBufferSize:     256,
			IdleTimeout:        30 * time.Second,
			NoPublisherTimeout: 30 * time.Second,
		},
	}

	srv := core.NewServer(cfg)
	apiMod := NewModule()
	webrtcMod := webrtcmod.NewModule()
	srv.RegisterModule(apiMod)
	srv.RegisterModule(webrtcMod)
	if err := srv.Init(); err != nil {
		t.Fatalf("server init: %v", err)
	}
	t.Cleanup(srv.Shutdown)
	cfg.WebRTC.Listen = webrtcMod.Addr().String()

	const streamKey = "live/whep-no-frames"
	stream, err := srv.StreamHub().GetOrCreate(streamKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.SetPublisher(&testPublisher{
		id:   "no-frame-publisher",
		info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264},
	}); err != nil {
		t.Fatal(err)
	}

	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(),
		append(chromedp.DefaultExecAllocatorOptions[:],
			chromedp.Flag("headless", true),
			chromedp.Flag("disable-gpu", true),
			chromedp.Flag("no-sandbox", true),
			chromedp.Flag("disable-dev-shm-usage", true),
			chromedp.Flag("autoplay-policy", "no-user-gesture-required"),
		)...,
	)
	defer allocCancel()
	browserCtx, browserCancel := chromedp.NewContext(allocCtx, chromedp.WithLogf(t.Logf))
	defer browserCancel()

	consoleURL := "http://" + apiMod.Addr().String() + "/console"
	if err := chromedp.Run(browserCtx,
		chromedp.Navigate(consoleURL),
		chromedp.WaitReady("body"),
		chromedp.WaitVisible(`button[data-action="stream-preview"]`, chromedp.ByQuery),
		chromedp.Click(`button[data-action="stream-preview"]`, chromedp.ByQuery),
		chromedp.WaitVisible(`.proto-tab[data-proto="whep-realtime"]`, chromedp.ByQuery),
		chromedp.Click(`.proto-tab[data-proto="whep-realtime"]`, chromedp.ByQuery),
	); err != nil {
		if strings.Contains(err.Error(), "websocket url timeout") || strings.Contains(err.Error(), "executable file not found") {
			t.Skipf("headless Chrome unavailable in this environment: %v", err)
		}
		t.Fatalf("open WHEP preview: %v", err)
	}

	time.Sleep(3 * time.Second)
	var status string
	if err := chromedp.Run(browserCtx, chromedp.TextContent("#player-status", &status)); err != nil {
		t.Fatalf("read player status: %v", err)
	}
	if strings.Contains(status, "Playing") {
		t.Fatalf("player status = %q without a decoded frame", status)
	}
}

func TestConsolePlaybackStatusRequiresClockProgressAndReportsStall(t *testing.T) {
	withConsoleBrowser(t, func(browserCtx context.Context) {
		var result struct {
			Initial string `json:"initial"`
			Proven  string `json:"proven"`
			Stalled string `json:"stalled"`
		}
		expression := `(new Promise(function(resolve) {
			var handlers = {};
			var fakeVideo = {
				readyState: 4,
				paused: false,
				currentTime: 10,
				videoWidth: 640,
				videoHeight: 480,
				error: null,
				addEventListener: function(name, fn) { handlers[name] = fn; },
				removeEventListener: function(name) { delete handlers[name]; },
				requestVideoFrameCallback: function(fn) { fn(); return 1; },
				cancelVideoFrameCallback: function() {}
			};
			streamMedia["live/status-test"] = {video_codec: "H265"};
			setPlayerStatus("Checking media progress...");
			monitorPlayableVideo(fakeVideo, playerGeneration, "TEST", "live/status-test");
			var initial = document.getElementById("player-status").textContent;
			fakeVideo.currentTime = 11;
			if (handlers.timeupdate) handlers.timeupdate();
			var proven = document.getElementById("player-status").textContent;
			setTimeout(function() {
				var stalled = document.getElementById("player-status").textContent;
				if (currentVideoEventCleanup) currentVideoEventCleanup();
				resolve({initial: initial, proven: proven, stalled: stalled});
			}, 3600);
		}))`
		if err := chromedp.Run(browserCtx, chromedp.Evaluate(expression, &result, func(params *runtime.EvaluateParams) *runtime.EvaluateParams {
			return params.WithAwaitPromise(true)
		})); err != nil {
			t.Fatalf("evaluate playback progress monitor: %v", err)
		}
		if strings.Contains(result.Initial, "Playing") {
			t.Fatalf("initial status = %q without media-clock progress", result.Initial)
		}
		if !strings.Contains(result.Proven, "Playing") {
			t.Fatalf("progressed status = %q, want Playing", result.Proven)
		}
		if !strings.Contains(result.Stalled, "stalled") {
			t.Fatalf("stalled status = %q, want explicit stalled state", result.Stalled)
		}
	})
}

func TestConsoleHLSPlayerEnablesLowLatencyMode(t *testing.T) {
	withConsoleBrowser(t, func(browserCtx context.Context) {
		var settings struct {
			LowLatencyMode bool `json:"lowLatencyMode"`
		}
		expression := `(function() {
			var previousHls = window.Hls;
			var captured = null;
			function FakeHls(options) {
				captured = options;
				this.loadSource = function() {};
				this.attachMedia = function() {};
				this.on = function() {};
				this.destroy = function() {};
			}
			FakeHls.isSupported = function() { return true; };
			FakeHls.Events = {MANIFEST_PARSED: "manifest", ERROR: "error"};

			try {
				window.Hls = FakeHls;
				endpoints.http = "127.0.0.1:8080";
				playHLS("live/llhls");
				return captured;
			} finally {
				destroyCurrentPlayer();
				if (previousHls === undefined) delete window.Hls;
				else window.Hls = previousHls;
			}
		})()`
		if err := chromedp.Run(browserCtx, chromedp.Evaluate(expression, &settings)); err != nil {
			t.Fatalf("exercise HLS player setup: %v", err)
		}
		if !settings.LowLatencyMode {
			t.Fatal("HLS player lowLatencyMode = false, want true")
		}
	})
}

func TestConsoleDASHPlayerUsesOneSegmentLiveDelay(t *testing.T) {
	withConsoleBrowser(t, func(browserCtx context.Context) {
		var delay struct {
			LiveDelayFragmentCount int      `json:"liveDelayFragmentCount"`
			LiveDelay              *float64 `json:"liveDelay"`
		}
		expression := `(function() {
			var previousDash = window.dashjs;
			var captured = null;
			var fakePlayer = {
				updateSettings: function(settings) { captured = settings; },
				initialize: function() {},
				on: function() {},
				reset: function() {}
			};
			function FakeMediaPlayer() {
				return {create: function() { return fakePlayer; }};
			}
			FakeMediaPlayer.events = {
				PLAYBACK_PLAYING: "playing",
				ERROR: "error"
			};

			try {
				window.dashjs = {MediaPlayer: FakeMediaPlayer};
				endpoints.http = "127.0.0.1:8080";
				playDASH("live/long-gop");
				return captured.streaming.delay;
			} finally {
				destroyCurrentPlayer();
				if (previousDash === undefined) delete window.dashjs;
				else window.dashjs = previousDash;
			}
		})()`
		if err := chromedp.Run(browserCtx, chromedp.Evaluate(expression, &delay)); err != nil {
			t.Fatalf("exercise DASH player setup: %v", err)
		}
		if delay.LiveDelayFragmentCount != 1 {
			t.Fatalf("DASH liveDelayFragmentCount = %d, want 1", delay.LiveDelayFragmentCount)
		}
		if delay.LiveDelay != nil && *delay.LiveDelay != 0 {
			t.Fatalf("DASH liveDelay = %.1f, want no fixed live delay", *delay.LiveDelay)
		}
	})
}

func TestConsoleWHEPStatsExposeAudioRTPPackets(t *testing.T) {
	withConsoleBrowser(t, func(browserCtx context.Context) {
		var text string
		expression := `(function() {
			var overlay = document.createElement("div");
			overlay.id = "webrtc-stats-overlay";
			document.body.appendChild(overlay);
			var video = {
				framesDecoded: 60, framesDropped: 0, keyFramesDecoded: 2,
				packetsLost: 0, packetsReceived: 200, bytesReceived: 100000,
				pliCount: 0, nackCount: 0, freezeCount: 0,
				totalFreezesDuration: 0, jitter: 0.01,
				jitterBufferDelay: 1, jitterBufferEmittedCount: 100
			};
			statsPrev = {
				v: Object.assign({}, video, {framesDecoded: 30, packetsReceived: 100, bytesReceived: 50000}),
				a: {packetsReceived: 100, packetsLost: 0, jitter: 0.01}
			};
			renderStatsOverlay(video, {packetsReceived: 130, packetsLost: 0, jitter: 0.02});
			return overlay.textContent;
		})()`
		if err := chromedp.Run(browserCtx, chromedp.Evaluate(expression, &text)); err != nil {
			t.Fatalf("render WHEP stats: %v", err)
		}
		if !strings.Contains(text, "A.Pkts30") {
			t.Fatalf("WHEP stats = %q, want audio RTP packet delta", text)
		}
	})
}

func TestConsoleFMP4MIMECandidatesFollowStreamCodecs(t *testing.T) {
	withConsoleBrowser(t, func(browserCtx context.Context) {
		var candidates []string
		expression := `(function() {
			streamMedia["live/h265-opus"] = {video_codec:"H265", audio_codec:"Opus"};
			return fmp4MimeCandidates("live/h265-opus");
		})()`
		if err := chromedp.Run(browserCtx, chromedp.Evaluate(expression, &candidates)); err != nil {
			t.Fatalf("evaluate H265+Opus FMP4 MIME candidates: %v", err)
		}
		if len(candidates) == 0 {
			t.Fatal("H265+Opus FMP4 MIME candidates are empty")
		}
		for _, candidate := range candidates {
			if strings.Contains(candidate, "avc1") || strings.Contains(candidate, "mp4a") {
				t.Fatalf("H265+Opus candidate incorrectly declares H264/AAC: %q", candidate)
			}
		}
		if !strings.Contains(candidates[0], "hvc1") || !strings.Contains(candidates[0], "opus") {
			t.Fatalf("first H265+Opus candidate = %q, want HEVC and Opus", candidates[0])
		}
	})
}

func TestConsoleFMP4UsesSegmentTimestamps(t *testing.T) {
	withConsoleBrowser(t, func(browserCtx context.Context) {
		var mode string
		expression := `(function() {
			var mediaSourceDescriptor = Object.getOwnPropertyDescriptor(window, "MediaSource");
			var oldCreateObjectURL = URL.createObjectURL;
			var oldRevokeObjectURL = URL.revokeObjectURL;
			var oldFetch = window.fetch;
				var sourceBuffer = {
					mode: "sequence",
				updating: false,
				addEventListener: function() {},
				abort: function() {},
				appendBuffer: function() {}
			};
			var mediaSource = {
				readyState: "closed",
				sourceBuffers: [],
				addEventListener: function(name, handler) {
					if (name === "sourceopen") {
						this.readyState = "open";
						handler();
					}
				},
				addSourceBuffer: function() {
					this.sourceBuffers.push(sourceBuffer);
					return sourceBuffer;
				},
				removeSourceBuffer: function(buffer) {
					this.sourceBuffers = this.sourceBuffers.filter(function(item) { return item !== buffer; });
				},
				endOfStream: function() {}
			};
			function FakeMediaSource() { return mediaSource; }
			FakeMediaSource.isTypeSupported = function() { return true; };

			try {
				Object.defineProperty(window, "MediaSource", {configurable: true, writable: true, value: FakeMediaSource});
				URL.createObjectURL = function() { return "blob:fmp4-test"; };
				URL.revokeObjectURL = function() {};
				window.fetch = function() { return new Promise(function() {}); };
				endpoints.http = "127.0.0.1:8080";
				streamMedia["live/h265-aac"] = {video_codec:"H265", audio_codec:"AAC"};
				playFMP4("live/h265-aac");
				return sourceBuffer.mode;
			} finally {
				destroyCurrentPlayer();
				if (mediaSourceDescriptor) Object.defineProperty(window, "MediaSource", mediaSourceDescriptor);
				else delete window.MediaSource;
				URL.createObjectURL = oldCreateObjectURL;
				URL.revokeObjectURL = oldRevokeObjectURL;
				window.fetch = oldFetch;
			}
		})()`
		if err := chromedp.Run(browserCtx, chromedp.Evaluate(expression, &mode)); err != nil {
			t.Fatalf("exercise FMP4 SourceBuffer setup: %v", err)
		}
		if mode != "segments" {
			t.Fatalf("FMP4 SourceBuffer mode = %q, want segments to preserve tfdt and signed CTS", mode)
		}
	})
}

func TestConsoleFMP4SourceBufferErrorTearsDownStream(t *testing.T) {
	withConsoleBrowser(t, func(browserCtx context.Context) {
		var result struct {
			AbortCalls              int  `json:"abortCalls"`
			ReaderCancelCalls       int  `json:"readerCancelCalls"`
			RemoveSourceBufferCalls int  `json:"removeSourceBufferCalls"`
			RevokeObjectURLCalls    int  `json:"revokeObjectURLCalls"`
			AppendCalls             int  `json:"appendCalls"`
			QueuedChunkAppended     bool `json:"queuedChunkAppended"`
		}
		expression := `(async function() {
			var mediaSourceDescriptor = Object.getOwnPropertyDescriptor(window, "MediaSource");
			var oldCreateObjectURL = URL.createObjectURL;
			var oldRevokeObjectURL = URL.revokeObjectURL;
			var oldFetch = window.fetch;
			var oldAbort = AbortController.prototype.abort;
			var abortCalls = 0;
			var readerCancelCalls = 0;
			var removeSourceBufferCalls = 0;
			var revokeObjectURLCalls = 0;
			var appendCalls = 0;
			var readCalls = 0;
			var sourceBufferListeners = {};
			var sourceBuffer = {
				mode: "sequence",
				updating: false,
				addEventListener: function(name, handler) { sourceBufferListeners[name] = handler; },
				abort: function() { this.updating = false; },
				appendBuffer: function() {
					appendCalls++;
					this.updating = true;
				}
			};
			var mediaSource = {
				readyState: "closed",
				sourceBuffers: [],
				addEventListener: function(name, handler) {
					if (name === "sourceopen") {
						this.readyState = "open";
						handler();
					}
				},
				addSourceBuffer: function() {
					this.sourceBuffers.push(sourceBuffer);
					return sourceBuffer;
				},
				removeSourceBuffer: function(buffer) {
					removeSourceBufferCalls++;
					this.sourceBuffers = this.sourceBuffers.filter(function(item) { return item !== buffer; });
				},
				endOfStream: function() {}
			};
			function FakeMediaSource() { return mediaSource; }
			FakeMediaSource.isTypeSupported = function() { return true; };

			try {
				Object.defineProperty(window, "MediaSource", {configurable: true, writable: true, value: FakeMediaSource});
				URL.createObjectURL = function() { return "blob:fmp4-error-test"; };
				URL.revokeObjectURL = function() { revokeObjectURLCalls++; };
				AbortController.prototype.abort = function() {
					abortCalls++;
					return oldAbort.call(this);
				};
				window.fetch = function() {
					return Promise.resolve({
						ok: true,
						body: {getReader: function() { return {
							read: function() {
								readCalls++;
								if (readCalls <= 2) return Promise.resolve({done: false, value: new Uint8Array([readCalls])});
								return new Promise(function() {});
							},
							cancel: function() { readerCancelCalls++; return Promise.resolve(); }
						};}}
					});
				};
				endpoints.http = "127.0.0.1:8080";
				streamMedia["live/h265-aac-error"] = {video_codec:"H265", audio_codec:"AAC"};
				playFMP4("live/h265-aac-error");
				for (var i = 0; i < 50 && readCalls < 3; i++) {
					await new Promise(function(resolve) { setTimeout(resolve, 2); });
				}
				if (readCalls < 3) throw new Error("FMP4 reader did not queue the second chunk");
				if (!sourceBufferListeners.error) throw new Error("SourceBuffer error listener was not installed");
				var activeGeneration = playerGeneration;
				sourceBufferListeners.error();
				await Promise.resolve();
				var appendCallsAfterError = appendCalls;
				playerGeneration = activeGeneration;
				currentMSEMediaSource = mediaSource;
				currentMSESourceBuffer = sourceBuffer;
				mediaSource.readyState = "open";
				mediaSource.sourceBuffers = [sourceBuffer];
				sourceBuffer.updating = false;
				if (sourceBufferListeners.updateend) sourceBufferListeners.updateend();
				await Promise.resolve();
				return {
					abortCalls: abortCalls,
					readerCancelCalls: readerCancelCalls,
					removeSourceBufferCalls: removeSourceBufferCalls,
					revokeObjectURLCalls: revokeObjectURLCalls,
					appendCalls: appendCalls,
					queuedChunkAppended: appendCalls !== appendCallsAfterError
				};
			} finally {
				destroyCurrentPlayer();
				if (mediaSourceDescriptor) Object.defineProperty(window, "MediaSource", mediaSourceDescriptor);
				else delete window.MediaSource;
				URL.createObjectURL = oldCreateObjectURL;
				URL.revokeObjectURL = oldRevokeObjectURL;
				window.fetch = oldFetch;
				AbortController.prototype.abort = oldAbort;
			}
		})()`
		if err := chromedp.Run(browserCtx, chromedp.Evaluate(expression, &result, func(params *runtime.EvaluateParams) *runtime.EvaluateParams {
			return params.WithAwaitPromise(true)
		})); err != nil {
			t.Fatalf("exercise FMP4 SourceBuffer error cleanup: %v", err)
		}
		if result.AbortCalls != 1 {
			t.Fatalf("AbortController.abort calls = %d, want 1", result.AbortCalls)
		}
		if result.ReaderCancelCalls != 1 {
			t.Fatalf("reader.cancel calls = %d, want 1", result.ReaderCancelCalls)
		}
		if result.RemoveSourceBufferCalls != 1 {
			t.Fatalf("removeSourceBuffer calls = %d, want 1", result.RemoveSourceBufferCalls)
		}
		if result.RevokeObjectURLCalls != 1 {
			t.Fatalf("revokeObjectURL calls = %d, want 1", result.RevokeObjectURLCalls)
		}
		if result.AppendCalls != 1 || result.QueuedChunkAppended {
			t.Fatalf("append calls after SourceBuffer error = %d, queued appended = %v; want queued chunk discarded", result.AppendCalls, result.QueuedChunkAppended)
		}
	})
}

func TestConsoleFMP4WaitsForQueuedAppendsBeforeEndOfStream(t *testing.T) {
	withConsoleBrowser(t, func(browserCtx context.Context) {
		var result struct {
			EOSBeforeDrain      int   `json:"eosBeforeDrain"`
			EOSAfterFirstUpdate int   `json:"eosAfterFirstUpdate"`
			EOSAfterDrain       int   `json:"eosAfterDrain"`
			AppendedChunks      []int `json:"appendedChunks"`
		}
		expression := `(async function() {
			var mediaSourceDescriptor = Object.getOwnPropertyDescriptor(window, "MediaSource");
			var oldCreateObjectURL = URL.createObjectURL;
			var oldRevokeObjectURL = URL.revokeObjectURL;
			var oldFetch = window.fetch;
			var sourceBufferListeners = {};
			var appendedChunks = [];
			var endOfStreamCalls = 0;
			var readCalls = 0;
			var sourceBuffer = {
				mode: "sequence",
				updating: false,
				addEventListener: function(name, handler) { sourceBufferListeners[name] = handler; },
				abort: function() { this.updating = false; },
				appendBuffer: function(chunk) {
					appendedChunks.push(chunk[0]);
					this.updating = true;
				}
			};
			var mediaSource = {
				readyState: "closed",
				sourceBuffers: [],
				addEventListener: function(name, handler) {
					if (name === "sourceopen") {
						this.readyState = "open";
						handler();
					}
				},
				addSourceBuffer: function() {
					this.sourceBuffers.push(sourceBuffer);
					return sourceBuffer;
				},
				removeSourceBuffer: function(buffer) {
					this.sourceBuffers = this.sourceBuffers.filter(function(item) { return item !== buffer; });
				},
				endOfStream: function() {
					endOfStreamCalls++;
					this.readyState = "ended";
				}
			};
			function FakeMediaSource() { return mediaSource; }
			FakeMediaSource.isTypeSupported = function() { return true; };

			try {
				Object.defineProperty(window, "MediaSource", {configurable: true, writable: true, value: FakeMediaSource});
				URL.createObjectURL = function() { return "blob:fmp4-eos-test"; };
				URL.revokeObjectURL = function() {};
				window.fetch = function() {
					return Promise.resolve({
						ok: true,
						body: {getReader: function() { return {
							read: function() {
								readCalls++;
								if (readCalls <= 2) return Promise.resolve({done: false, value: new Uint8Array([readCalls])});
								return Promise.resolve({done: true});
							},
							cancel: function() { return Promise.resolve(); }
						};}}
					});
				};
				endpoints.http = "127.0.0.1:8080";
				streamMedia["live/h265-aac-eos"] = {video_codec:"H265", audio_codec:"AAC"};
				playFMP4("live/h265-aac-eos");
				for (var i = 0; i < 50 && readCalls < 3; i++) {
					await new Promise(function(resolve) { setTimeout(resolve, 2); });
				}
				if (readCalls < 3) throw new Error("FMP4 reader did not reach EOF");
				var eosBeforeDrain = endOfStreamCalls;
				sourceBuffer.updating = false;
				if (sourceBufferListeners.updateend) sourceBufferListeners.updateend();
				var eosAfterFirstUpdate = endOfStreamCalls;
				sourceBuffer.updating = false;
				if (sourceBufferListeners.updateend) sourceBufferListeners.updateend();
				return {
					eosBeforeDrain: eosBeforeDrain,
					eosAfterFirstUpdate: eosAfterFirstUpdate,
					eosAfterDrain: endOfStreamCalls,
					appendedChunks: appendedChunks
				};
			} finally {
				destroyCurrentPlayer();
				if (mediaSourceDescriptor) Object.defineProperty(window, "MediaSource", mediaSourceDescriptor);
				else delete window.MediaSource;
				URL.createObjectURL = oldCreateObjectURL;
				URL.revokeObjectURL = oldRevokeObjectURL;
				window.fetch = oldFetch;
			}
		})()`
		if err := chromedp.Run(browserCtx, chromedp.Evaluate(expression, &result, func(params *runtime.EvaluateParams) *runtime.EvaluateParams {
			return params.WithAwaitPromise(true)
		})); err != nil {
			t.Fatalf("exercise queued FMP4 end-of-stream handling: %v", err)
		}
		if result.EOSBeforeDrain != 0 {
			t.Fatalf("endOfStream calls at reader EOF = %d, want 0 while append work remains", result.EOSBeforeDrain)
		}
		if result.EOSAfterFirstUpdate != 0 {
			t.Fatalf("endOfStream calls after first update = %d, want 0 while queued chunk is appending", result.EOSAfterFirstUpdate)
		}
		if result.EOSAfterDrain != 1 {
			t.Fatalf("endOfStream calls after queue drain = %d, want 1", result.EOSAfterDrain)
		}
		if len(result.AppendedChunks) != 2 || result.AppendedChunks[0] != 1 || result.AppendedChunks[1] != 2 {
			t.Fatalf("appended chunks = %v, want [1 2]", result.AppendedChunks)
		}
	})
}

func TestConsoleFMP4StartsAtFirstBufferedRange(t *testing.T) {
	withConsoleBrowser(t, func(browserCtx context.Context) {
		var currentTime float64
		expression := `(async function() {
			var mediaSourceDescriptor = Object.getOwnPropertyDescriptor(window, "MediaSource");
			var oldCreateObjectURL = URL.createObjectURL;
			var oldRevokeObjectURL = URL.revokeObjectURL;
			var oldFetch = window.fetch;
			var video = document.getElementById("player-video");
			var currentTimeDescriptor = Object.getOwnPropertyDescriptor(video, "currentTime");
			var readyStateDescriptor = Object.getOwnPropertyDescriptor(video, "readyState");
			var bufferedDescriptor = Object.getOwnPropertyDescriptor(video, "buffered");
			var oldPlay = video.play;
			var playbackTime = 0;
			var playCalls = 0;
			var sourceBufferListeners = {};
			var sourceBuffer = {
				mode: "segments",
				updating: false,
				addEventListener: function(name, handler) { sourceBufferListeners[name] = handler; },
				abort: function() {},
				appendBuffer: function() {
					var self = this;
					self.updating = true;
					setTimeout(function() {
						self.updating = false;
						if (sourceBufferListeners.updateend) sourceBufferListeners.updateend();
					}, 0);
				}
			};
			var mediaSource = {
				readyState: "closed",
				sourceBuffers: [],
				addEventListener: function(name, handler) {
					if (name === "sourceopen") {
						this.readyState = "open";
						handler();
					}
				},
				addSourceBuffer: function() {
					this.sourceBuffers.push(sourceBuffer);
					return sourceBuffer;
				},
				removeSourceBuffer: function(buffer) {
					this.sourceBuffers = this.sourceBuffers.filter(function(item) { return item !== buffer; });
				},
				endOfStream: function() {}
			};
			function FakeMediaSource() { return mediaSource; }
			FakeMediaSource.isTypeSupported = function() { return true; };

			try {
				Object.defineProperty(window, "MediaSource", {configurable: true, writable: true, value: FakeMediaSource});
				URL.createObjectURL = function() { return "blob:fmp4-playback-test"; };
				URL.revokeObjectURL = function() {};
				Object.defineProperty(video, "currentTime", {
					configurable: true,
					get: function() { return playbackTime; },
					set: function(value) { playbackTime = value; }
				});
				Object.defineProperty(video, "readyState", {configurable: true, get: function() { return 2; }});
				Object.defineProperty(video, "buffered", {configurable: true, get: function() {
					return {length: 1, start: function() { return 0.035; }, end: function() { return 0.235; }};
				}});
				video.play = function() { playCalls++; return Promise.resolve(); };
				window.fetch = function() {
					var reads = 0;
					return Promise.resolve({
						ok: true,
						body: {getReader: function() { return {
							read: function() {
								reads++;
								if (reads === 1) return Promise.resolve({done: false, value: new Uint8Array([1, 2, 3])});
								return new Promise(function() {});
							},
							cancel: function() { return Promise.resolve(); }
						};}}
					});
				};
				endpoints.http = "127.0.0.1:8080";
				streamMedia["live/h265-aac"] = {video_codec:"H265", audio_codec:"AAC"};
				playFMP4("live/h265-aac");
				for (var i = 0; i < 50 && playCalls === 0; i++) {
					await new Promise(function(resolve) { setTimeout(resolve, 2); });
				}
				if (playCalls === 0) throw new Error("FMP4 playback did not start after updateend");
				return playbackTime;
			} finally {
				destroyCurrentPlayer();
				if (mediaSourceDescriptor) Object.defineProperty(window, "MediaSource", mediaSourceDescriptor);
				else delete window.MediaSource;
				URL.createObjectURL = oldCreateObjectURL;
				URL.revokeObjectURL = oldRevokeObjectURL;
				window.fetch = oldFetch;
				video.play = oldPlay;
				if (currentTimeDescriptor) Object.defineProperty(video, "currentTime", currentTimeDescriptor);
				else delete video.currentTime;
				if (readyStateDescriptor) Object.defineProperty(video, "readyState", readyStateDescriptor);
				else delete video.readyState;
				if (bufferedDescriptor) Object.defineProperty(video, "buffered", bufferedDescriptor);
				else delete video.buffered;
			}
		})()`
		if err := chromedp.Run(browserCtx, chromedp.Evaluate(expression, &currentTime, func(params *runtime.EvaluateParams) *runtime.EvaluateParams {
			return params.WithAwaitPromise(true)
		})); err != nil {
			t.Fatalf("exercise first FMP4 append: %v", err)
		}
		if currentTime != 0.035 {
			t.Fatalf("FMP4 playback currentTime = %.3f, want first buffered timestamp 0.035", currentTime)
		}
	})
}
