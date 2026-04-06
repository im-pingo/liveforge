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
)

// TestConsolePublishFlow launches headless Chrome on the console page
// (served over localhost HTTP — a secure context) and exercises the full
// WebRTC Publish workflow:
//   1. Open the publish modal
//   2. Verify fake camera/mic devices are enumerated
//   3. Enter a stream key and click "Start Publishing"
//   4. Verify the WHIP session succeeds and the stream appears in the hub
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
