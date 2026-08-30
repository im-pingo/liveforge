//go:build audiocodec

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/im-pingo/liveforge/module/gb28181"
	"github.com/im-pingo/liveforge/module/sipgateway"
	webrtcmod "github.com/im-pingo/liveforge/module/webrtc"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/tools/testkit/push"
	"github.com/im-pingo/liveforge/tools/testkit/source"
	"github.com/im-pingo/liveforge/tools/testkit/testutil"
)

func TestSIPGB28181WHIPBrowserBridgeMatrix(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping real browser protocol bridge matrix in short mode")
	}
	allocator, cancelAllocator := chromedp.NewExecAllocator(context.Background(),
		append(chromedp.DefaultExecAllocatorOptions[:],
			chromedp.Flag("headless", true),
			chromedp.Flag("disable-gpu", true),
			chromedp.Flag("no-sandbox", true),
			chromedp.Flag("disable-dev-shm-usage", true),
			chromedp.Flag("autoplay-policy", "no-user-gesture-required"),
		)...,
	)
	defer cancelAllocator()
	chromium := &matrixChromiumAvailability{}

	t.Run("sip_publish_to_gb28181_and_whep", func(t *testing.T) {
		srv, sipModule, gbModule := newProtocolMatrixServer(t)
		streamKey := "matrix/sip-publish"
		published, err := sipModule.StartLabSession(context.Background(), sipgateway.LabSessionRequest{
			Mode:      sipgateway.LabModePublish,
			DeviceID:  "matrix-sip-publisher",
			StreamKey: streamKey,
			Codec:     "PCMA",
		})
		if err != nil {
			t.Fatalf("start SIP publish lab: %v", err)
		}
		t.Cleanup(func() { _ = sipModule.StopLabSession(published.ID) })
		waitForMatrixStream(t, srv, streamKey, avframe.CodecG711A)

		received, err := gbModule.StartLabSession(context.Background(), gb28181.LabSessionRequest{
			Mode:      gb28181.LabModeReceive,
			DeviceID:  "34020000001320000101",
			ChannelID: "34020000001320000102",
			StreamKey: streamKey,
		})
		if err != nil {
			t.Fatalf("start GB28181 receive lab: %v", err)
		}
		t.Cleanup(func() { _ = gbModule.StopLabSession(received.ID) })
		waitForGBMatrixReceive(t, gbModule, received.ID)
		runMatrixWHEPBrowser(t, allocator, chromium, srv.WebRTCAddr(), streamKey, 160, 90)
	})

	t.Run("gb28181_publish_to_sip_and_whep", func(t *testing.T) {
		srv, sipModule, gbModule := newProtocolMatrixServer(t)
		streamKey := "matrix/gb-publish"
		published, err := gbModule.StartLabSession(context.Background(), gb28181.LabSessionRequest{
			Mode:      gb28181.LabModePublish,
			DeviceID:  "34020000001320000111",
			ChannelID: "34020000001320000112",
			StreamKey: streamKey,
		})
		if err != nil {
			t.Fatalf("start GB28181 publish lab: %v", err)
		}
		t.Cleanup(func() { _ = gbModule.StopLabSession(published.ID) })
		waitForMatrixStream(t, srv, streamKey, avframe.CodecG711A)

		received, err := sipModule.StartLabSession(context.Background(), sipgateway.LabSessionRequest{
			Mode:      sipgateway.LabModeReceive,
			DeviceID:  "matrix-sip-receiver-gb",
			StreamKey: streamKey,
			Codec:     "PCMU",
		})
		if err != nil {
			t.Fatalf("start SIP receive lab: %v", err)
		}
		t.Cleanup(func() { _ = sipModule.StopLabSession(received.ID) })
		waitForSIPMatrixReceive(t, sipModule, received.ID)
		runMatrixWHEPBrowser(t, allocator, chromium, srv.WebRTCAddr(), streamKey, 160, 90)
	})

	t.Run("whip_publish_to_sip_gb28181_and_whep", func(t *testing.T) {
		srv, sipModule, gbModule := newProtocolMatrixServer(t)
		streamKey := "matrix/whip-publish"
		ctx, cancel := context.WithCancel(context.Background())
		pushDone := make(chan error, 1)
		pusher, err := push.NewPusher("whip")
		if err != nil {
			t.Fatalf("create WHIP pusher: %v", err)
		}
		go func() {
			_, pushErr := pusher.Push(ctx, source.NewFLVSourceLoop(0), push.PushConfig{
				Protocol: "whip",
				Target:   fmt.Sprintf("http://%s/webrtc/whip/%s", srv.WebRTCAddr(), streamKey),
				Duration: 0,
				Realtime: true,
			})
			pushDone <- pushErr
		}()
		t.Cleanup(func() {
			cancel()
			select {
			case <-pushDone:
			case <-time.After(3 * time.Second):
				t.Error("WHIP pusher did not stop after cancellation")
			}
		})
		waitForMatrixStream(t, srv, streamKey, avframe.CodecOpus)

		sipReceived, err := sipModule.StartLabSession(context.Background(), sipgateway.LabSessionRequest{
			Mode:      sipgateway.LabModeReceive,
			DeviceID:  "matrix-sip-receiver-whip",
			StreamKey: streamKey,
			Codec:     "PCMA",
		})
		if err != nil {
			t.Fatalf("start SIP receive lab for WHIP: %v", err)
		}
		t.Cleanup(func() { _ = sipModule.StopLabSession(sipReceived.ID) })
		gbReceived, err := gbModule.StartLabSession(context.Background(), gb28181.LabSessionRequest{
			Mode:      gb28181.LabModeReceive,
			DeviceID:  "34020000001320000121",
			ChannelID: "34020000001320000122",
			StreamKey: streamKey,
		})
		if err != nil {
			t.Fatalf("start GB28181 receive lab for WHIP: %v", err)
		}
		t.Cleanup(func() { _ = gbModule.StopLabSession(gbReceived.ID) })
		waitForSIPMatrixReceive(t, sipModule, sipReceived.ID)
		waitForGBMatrixReceive(t, gbModule, gbReceived.ID)
		runMatrixWHEPBrowser(t, allocator, chromium, srv.WebRTCAddr(), streamKey, 640, 360)
	})
}

func newProtocolMatrixServer(t *testing.T) (*testutil.TestServer, *sipgateway.Module, *gb28181.Module) {
	t.Helper()
	srv := testutil.StartTestServer(t,
		testutil.WithSIP(),
		testutil.WithGB28181(),
		testutil.WithSIPGateway(),
		testutil.WithWebRTC(),
		testutil.WithAudioCodec(),
	)
	sipModule, ok := srv.ModuleByName("sipgateway").(*sipgateway.Module)
	if !ok {
		t.Fatal("test server did not expose SIP gateway module")
	}
	gbModule, ok := srv.ModuleByName("gb28181").(*gb28181.Module)
	if !ok {
		t.Fatal("test server did not expose GB28181 module")
	}
	return srv, sipModule, gbModule
}

func waitForMatrixStream(t *testing.T, srv *testutil.TestServer, streamKey string, audio avframe.CodecType) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if srv.StreamHasVideoGOP(streamKey) && srv.StreamHasAudio(streamKey, audio) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("stream %q did not expose H.264 GOP plus %s audio", streamKey, audio)
}

func waitForSIPMatrixReceive(t *testing.T, module *sipgateway.Module, id string) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		for _, snapshot := range module.ListLabSessions() {
			if snapshot.ID != id {
				continue
			}
			if snapshot.State == sipgateway.LabSessionStateFailed {
				t.Fatalf("SIP receive lab failed: %s", snapshot.LastError)
			}
			if snapshot.State == sipgateway.LabSessionStateActive &&
				snapshot.AudioRTPPacketsRecv > 0 && snapshot.VideoRTPPacketsRecv > 0 && snapshot.RTCPPacketsRecv > 0 {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("SIP receive lab %s did not receive audio, video, and RTCP", id)
}

func waitForGBMatrixReceive(t *testing.T, module *gb28181.Module, id string) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		for _, snapshot := range module.ListLabSessions() {
			if snapshot.ID != id {
				continue
			}
			if snapshot.State == gb28181.LabSessionStateFailed {
				t.Fatalf("GB28181 receive lab failed: %s", snapshot.LastError)
			}
			if snapshot.State == gb28181.LabSessionStateActive && snapshot.RTPPacketsRecv > 0 &&
				snapshot.RTCPPacketsRecv > 0 && snapshot.PSFramesRecv > 0 &&
				snapshot.AudioFramesRecv > 0 && snapshot.VideoFramesRecv > 0 {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("GB28181 receive lab %s did not receive RTP, RTCP, PS, audio, and video", id)
}

type matrixBrowserProbe struct {
	ReadyState      int     `json:"readyState"`
	Width           int     `json:"width"`
	Height          int     `json:"height"`
	CurrentTime     float64 `json:"currentTime"`
	Error           string  `json:"error"`
	ICE             string  `json:"ice"`
	ConnectError    string  `json:"connectError"`
	Stage           string  `json:"stage"`
	SessionLocation string  `json:"sessionLocation"`
	VideoPackets    uint64  `json:"videoPackets"`
	AudioPackets    uint64  `json:"audioPackets"`
	FramesDecoded   uint64  `json:"framesDecoded"`
}

type matrixChromiumAvailability struct {
	established bool
}

func (a *matrixChromiumAvailability) canSkip(err error) bool {
	if a == nil || a.established || err == nil {
		return false
	}
	return strings.Contains(err.Error(), "websocket url timeout") ||
		strings.Contains(err.Error(), "executable file not found")
}

func (a *matrixChromiumAvailability) markEstablished() {
	if a != nil {
		a.established = true
	}
}

func runMatrixWHEPBrowser(t *testing.T, allocator context.Context, chromium *matrixChromiumAvailability, whepAddr, streamKey string, width, height int) {
	t.Helper()
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(matrixWHEPPlayerHTML("http://"+whepAddr, streamKey)))
	}))
	defer page.Close()
	browser, cancelBrowser := chromedp.NewContext(allocator, chromedp.WithLogf(t.Logf))
	defer cancelBrowser()
	if err := chromedp.Run(browser, chromedp.Navigate(page.URL)); err != nil {
		if chromium.canSkip(err) {
			t.Skipf("headless Chrome unavailable: %v", err)
		}
		t.Fatalf("navigate WHEP matrix player: %v", err)
	}
	chromium.markEstablished()
	if err := chromedp.Run(browser, chromedp.Evaluate(`void window.__connectMatrix(); true`, nil)); err != nil {
		t.Fatalf("start WHEP matrix player: %v", err)
	}

	first := waitForMatrixBrowserProbe(t, browser, func(probe matrixBrowserProbe) bool {
		return probe.ReadyState >= 3 && probe.Width == width && probe.Height == height &&
			probe.CurrentTime > 0.2 && probe.VideoPackets > 0 && probe.AudioPackets > 0 &&
			probe.FramesDecoded > 0 && (probe.ICE == "connected" || probe.ICE == "completed")
	})
	time.Sleep(1200 * time.Millisecond)
	second := waitForMatrixBrowserProbe(t, browser, func(probe matrixBrowserProbe) bool {
		return probe.CurrentTime > first.CurrentTime+0.3 && probe.VideoPackets > first.VideoPackets &&
			probe.AudioPackets > first.AudioPackets && probe.FramesDecoded > first.FramesDecoded
	})
	if second.Error != "" {
		t.Fatalf("browser media error after playback advance: %s", second.Error)
	}
	soakDeadline := time.Now().Add(protocolMatrixSoakDuration(t))
	for time.Now().Before(soakDeadline) {
		previous := second
		time.Sleep(time.Second)
		second = waitForMatrixBrowserProbe(t, browser, func(probe matrixBrowserProbe) bool {
			return probe.CurrentTime > previous.CurrentTime+0.3 && probe.VideoPackets > previous.VideoPackets &&
				probe.AudioPackets > previous.AudioPackets && probe.FramesDecoded > previous.FramesDecoded
		})
	}
	assertMatrixWHEPStatus(t, whepAddr, second.SessionLocation)
}

func protocolMatrixSoakDuration(t *testing.T) time.Duration {
	t.Helper()
	value := strings.TrimSpace(os.Getenv("LIVEFORGE_PROTOCOL_MATRIX_SOAK"))
	if value == "" {
		return 0
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration < 0 {
		t.Fatalf("LIVEFORGE_PROTOCOL_MATRIX_SOAK=%q is not a non-negative duration", value)
	}
	return duration
}

func waitForMatrixBrowserProbe(t *testing.T, browser context.Context, accept func(matrixBrowserProbe) bool) matrixBrowserProbe {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	var probe matrixBrowserProbe
	for time.Now().Before(deadline) {
		probeCtx, cancel := context.WithTimeout(browser, 2*time.Second)
		err := chromedp.Run(probeCtx, chromedp.Evaluate(`window.__probeMatrix()`, &probe))
		cancel()
		if err == nil {
			if probe.ConnectError != "" {
				t.Fatalf("WHEP browser connection failed at %s: %s", probe.Stage, probe.ConnectError)
			}
			if probe.Error != "" {
				t.Fatalf("WHEP browser media error: %s", probe.Error)
			}
			if accept(probe) {
				return probe
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("WHEP browser media did not advance: %+v", probe)
	return matrixBrowserProbe{}
}

func assertMatrixWHEPStatus(t *testing.T, whepAddr, location string) {
	t.Helper()
	if location == "" {
		t.Fatal("WHEP response did not expose a session Location")
	}
	type response struct {
		Feed webrtcmod.WHEPFeedStatus `json:"feed"`
	}
	deadline := time.Now().Add(5 * time.Second)
	client := &http.Client{Timeout: 500 * time.Millisecond}
	var status response
	for time.Now().Before(deadline) {
		requestCtx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		statusCode, err := requestMatrixWHEPStatus(requestCtx, client, "http://"+whepAddr+location+"/status", &status)
		cancel()
		if err == nil && statusCode == http.StatusOK {
			if status.Feed.State == webrtcmod.WHEPFeedMediaStalled {
				t.Fatalf("WHEP server status entered media_stalled: %+v", status.Feed)
			}
			if status.Feed.State == webrtcmod.WHEPFeedPlaying && status.Feed.ExpectedVideo && status.Feed.ExpectedAudio &&
				status.Feed.VideoFrames > 0 && status.Feed.AudioFrames > 0 && status.Feed.RTPPacketsSent > 0 &&
				status.Feed.RTCPPacketsReceived > 0 {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("WHEP server status did not confirm advancing audio/video RTP/RTCP: %+v", status.Feed)
}

func requestMatrixWHEPStatus(ctx context.Context, client *http.Client, url string, target any) (int, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	if err := json.NewDecoder(response.Body).Decode(target); err != nil {
		return response.StatusCode, err
	}
	return response.StatusCode, nil
}

func matrixWHEPPlayerHTML(whepBase, streamKey string) string {
	return `<!doctype html><html><body>
<video id="video" autoplay muted playsinline></video>
<script>
const video = document.getElementById('video');
let pc = null;
let media = new MediaStream();
let latest = {videoPackets:0,audioPackets:0,framesDecoded:0};
window.__matrixStage = 'idle';
async function connectMatrix() {
  window.__matrixStage = 'creating_pc';
  pc = new RTCPeerConnection();
  video.srcObject = media;
  pc.ontrack = event => { media.addTrack(event.track); video.srcObject = media; video.play().catch(() => {}); };
  pc.oniceconnectionstatechange = () => { window.__matrixICE = pc.iceConnectionState; };
  pc.addTransceiver('video', {direction:'recvonly'});
  pc.addTransceiver('audio', {direction:'recvonly'});
  const offer = await pc.createOffer();
  await pc.setLocalDescription(offer);
  window.__matrixStage = 'gathering';
  await new Promise(resolve => {
    if (pc.iceGatheringState === 'complete') { resolve(); return; }
    const timer = setTimeout(resolve, 2000);
    pc.onicegatheringstatechange = () => { if (pc.iceGatheringState === 'complete') { clearTimeout(timer); resolve(); } };
  });
  window.__matrixStage = 'posting';
  const response = await fetch('` + whepBase + `/webrtc/whep/` + streamKey + `?mode=live', {
    method:'POST', headers:{'Content-Type':'application/sdp'}, body:pc.localDescription.sdp
  });
  if (!response.ok) throw new Error('WHEP ' + response.status + ': ' + await response.text());
  window.__matrixSession = response.headers.get('Location') || '';
  await pc.setRemoteDescription({type:'answer', sdp:await response.text()});
  window.__matrixStage = 'remote_set';
  setInterval(async () => {
    const stats = await pc.getStats();
    const next = {videoPackets:0,audioPackets:0,framesDecoded:0};
    stats.forEach(report => {
      if (report.type !== 'inbound-rtp') return;
      if (report.kind === 'video') {
        next.videoPackets += report.packetsReceived || 0;
        next.framesDecoded += report.framesDecoded || 0;
      }
      if (report.kind === 'audio') next.audioPackets += report.packetsReceived || 0;
    });
    latest = next;
  }, 100);
}
window.__connectMatrix = () => connectMatrix().catch(error => { window.__matrixError = String(error); });
window.__probeMatrix = () => ({
  readyState:video.readyState, width:video.videoWidth, height:video.videoHeight,
  currentTime:video.currentTime, error:video.error ? String(video.error.code) : '',
  ice:window.__matrixICE || '', connectError:window.__matrixError || '',
  stage:window.__matrixStage || '', sessionLocation:window.__matrixSession || '',
  videoPackets:latest.videoPackets, audioPackets:latest.audioPackets, framesDecoded:latest.framesDecoded
});
</script></body></html>`
}

func TestMatrixChromiumEnvironmentalSkipEndsAfterAvailabilityIsEstablished(t *testing.T) {
	var availability matrixChromiumAvailability
	startupFailure := errors.New("websocket url timeout")
	if !availability.canSkip(startupFailure) {
		t.Fatal("initial Chromium startup failure must remain an environmental skip")
	}
	availability.markEstablished()
	if availability.canSkip(startupFailure) {
		t.Fatal("Chromium startup failure was still skippable after a successful matrix launch")
	}
}

func TestMatrixWHEPStatusRequestIsBounded(t *testing.T) {
	requestCanceled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		close(requestCanceled)
	}))
	defer server.Close()

	client := &http.Client{Timeout: 50 * time.Millisecond}
	requestCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	var status map[string]any
	if _, err := requestMatrixWHEPStatus(requestCtx, client, server.URL, &status); err == nil {
		t.Fatal("hanging WHEP status request returned no timeout error")
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("hanging WHEP status request returned after %s, want a bounded call", elapsed)
	}
	select {
	case <-requestCanceled:
	case <-time.After(time.Second):
		t.Fatal("WHEP status request timeout did not cancel the HTTP request context")
	}
}
