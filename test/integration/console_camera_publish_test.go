package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/im-pingo/liveforge/module/api"
	"github.com/im-pingo/liveforge/module/gb28181"
	"github.com/im-pingo/liveforge/module/sipgateway"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/tools/testkit/testutil"
)

// TestConsoleCameraProtocolPublish catches the Console substituting generated
// lab media for the selected camera/microphone, falling back to a non-G.711
// capture codec, or leaking capture and protocol resources after Stop.
// It deliberately runs without audiocodec: browser H.264/G.711 needs no native
// transcoder. Chrome's fake devices exercise real getUserMedia without hardware.
func TestConsoleCameraProtocolPublish(t *testing.T) {
	if testing.Short() {
		if os.Getenv("LIVEFORGE_REQUIRE_BROWSER") == "1" {
			t.Fatal("required camera publishing browser regression cannot run with -short")
		}
		t.Skip("skipping camera publishing browser regression in short mode")
	}
	allocator, cancelAllocator := chromedp.NewExecAllocator(context.Background(), append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(os.Getenv("CHROME_BIN")),
		chromedp.Flag("headless", true),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("autoplay-policy", "no-user-gesture-required"),
		chromedp.Flag("use-fake-device-for-media-stream", true),
		chromedp.Flag("use-fake-ui-for-media-stream", true),
	)...)
	defer cancelAllocator()
	browserEstablished := false

	for _, tc := range []struct {
		name, protocol, codec string
		audio                 avframe.CodecType
	}{
		{"sip_pcma", "sip", "PCMA", avframe.CodecG711A},
		{"sip_pcmu", "sip", "PCMU", avframe.CodecG711U},
		{"gb28181_pcma", "gb28181", "PCMA", avframe.CodecG711A},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := testutil.StartTestServer(t, testutil.WithSIP(), testutil.WithSIPGateway(),
				testutil.WithGB28181(), testutil.WithWebRTC(), testutil.WithAPI())
			sipModule := srv.ModuleByName("sipgateway").(*sipgateway.Module)
			gbModule := srv.ModuleByName("gb28181").(*gb28181.Module)
			t.Cleanup(func() {
				for _, session := range sipModule.ListLabSessions() {
					_ = sipModule.StopLabSession(session.ID)
				}
				for _, session := range gbModule.ListLabSessions() {
					_ = gbModule.StopLabSession(session.ID)
				}
			})
			browser, cancelBrowser := chromedp.NewContext(allocator)
			defer cancelBrowser()
			browser, cancelDeadline := context.WithTimeout(browser, 50*time.Second)
			defer cancelDeadline()
			if err := chromedp.Run(browser,
				chromedp.ActionFunc(func(ctx context.Context) error {
					_, err := page.AddScriptToEvaluateOnNewDocument(`
window.__cameraTracks = [];
const originalGetUserMedia = navigator.mediaDevices.getUserMedia.bind(navigator.mediaDevices);
navigator.mediaDevices.getUserMedia = async function(constraints) {
  const stream = await originalGetUserMedia(constraints);
  window.__cameraTracks.push(...stream.getTracks());
  return stream;
};
window.__cameraDeletes = [];
window.__cameraWhipResources = [];
const originalFetch = window.fetch.bind(window);
window.fetch = async function(input, options) {
  const response = await originalFetch(input, options);
  const url = new URL(input instanceof Request ? input.url : String(input), location.href).href;
  const method = (options && options.method || (input instanceof Request && input.method) || 'GET').toUpperCase();
  if (method === 'POST' && url.includes('/webrtc/whip/') && response.ok) {
    window.__cameraWhipResources.push(new URL(response.headers.get('Location'), url).href);
  }
  if (method === 'DELETE') {
    response.clone().text().then(body => window.__cameraDeletes.push({url, status:response.status, body}));
  }
  return response;
};`).Do(ctx)
					return err
				}),
				chromedp.Navigate("http://"+srv.APIAddr()+"/console/publish"),
				chromedp.WaitReady("body"),
			); err != nil {
				if !browserEstablished && os.Getenv("LIVEFORGE_REQUIRE_BROWSER") != "1" &&
					(strings.Contains(err.Error(), "websocket url timeout") || strings.Contains(err.Error(), "executable file not found") || os.IsNotExist(err)) {
					t.Skipf("headless Chrome unavailable: %v", err)
				}
				t.Fatalf("open camera publishing Console: %v", err)
			}
			browserEstablished = true
			var codecs struct{ H264, Audio bool }
			cameraEvaluate(t, browser, fmt.Sprintf(`({
H264: RTCRtpSender.getCapabilities('video').codecs.some(c => c.mimeType.toLowerCase() === 'video/h264') &&
      RTCRtpReceiver.getCapabilities('video').codecs.some(c => c.mimeType.toLowerCase() === 'video/h264'),
Audio: RTCRtpSender.getCapabilities('audio').codecs.some(c => c.mimeType.toLowerCase() === 'audio/%s')
})`, strings.ToLower(tc.codec)), &codecs)
			if !codecs.H264 || !codecs.Audio {
				if os.Getenv("LIVEFORGE_REQUIRE_BROWSER") == "1" {
					t.Fatalf("required browser does not support H.264/%s: %+v", tc.codec, codecs)
				}
				t.Skipf("browser does not support H.264/%s: %+v", tc.codec, codecs)
			}
			streamKey := "camera/" + tc.name
			previousSource := ""
			for cycle := 0; cycle < 2; cycle++ {
				if cycle == 0 {
					cameraEvaluate(t, browser, fmt.Sprintf(`(() => {
const protocol = document.getElementById('publish-protocol');
protocol.value = %q; protocol.dispatchEvent(new Event('change', {bubbles:true}));
const values = %s;
Object.entries(values).forEach(([id, value]) => {
  const element = document.getElementById(id); element.value = value;
  element.dispatchEvent(new Event('input', {bubbles:true}));
});
return true;
})()`, tc.protocol, cameraFields(tc.protocol, tc.codec, streamKey)), nil)
					cameraEventually(t, browser, "selected camera and microphone preview", func() bool {
						var ready bool
						cameraEvaluate(t, browser, `(() => {
const preview = document.getElementById('publish-preview');
return document.getElementById('publish-video-device').options.length > 0 &&
  document.getElementById('publish-audio-device').options.length > 0 &&
  preview.videoWidth > 160 && preview.videoHeight > 90 && preview.srcObject &&
  preview.srcObject.getVideoTracks().some(t => t.readyState === 'live') &&
  preview.srcObject.getAudioTracks().some(t => t.readyState === 'live') &&
  !document.getElementById('btn-start-publish').disabled;
})()`, &ready)
						return ready
					})
				}
				// Restart with the same target, identity and selected devices, using
				// Start directly after Stop; no page reload or selector change may
				// reacquire the capture or hide a leaked protocol identity.
				cameraEvaluate(t, browser, `document.getElementById('btn-start-publish').click()`, nil)

				var lab cameraLabSnapshot
				cameraEventually(t, browser, "camera-backed protocol publish session", func() bool {
					lab = cameraLab(t, srv, tc.protocol, streamKey)
					if lab.State == "failed" {
						t.Fatalf("protocol session failed: %+v", lab)
					}
					return lab.State == "active"
				})
				if !strings.HasPrefix(lab.Source, "_console/") || lab.Source == streamKey {
					t.Fatalf("protocol publish did not use a distinct browser source: %+v", lab)
				}
				if lab.Source == previousSource {
					t.Fatalf("restart reused retired browser source %q", lab.Source)
				}
				previousSource = lab.Source
				var firstSource, firstTarget api.StreamInfo
				cameraEventually(t, browser, "browser H.264/G.711 on source and protocol target", func() bool {
					firstSource = cameraStream(t, srv, lab.Source)
					firstTarget = cameraStream(t, srv, streamKey)
					return cameraStreamReady(firstSource, tc.audio) && cameraStreamReady(firstTarget, tc.audio) &&
						srv.StreamHasVideoGOP(lab.Source) && srv.StreamHasVideoGOP(streamKey)
				})

				cameraEvaluate(t, browser, cameraTargetPlayer("http://"+srv.WebRTCAddr(), streamKey), nil)
				var firstPlayback cameraPlaybackProbe
				cameraEventually(t, browser, "decoded protocol target matching the captured camera", func() bool {
					firstPlayback = cameraPlayback(t, browser)
					return firstPlayback.Width > 160 && firstPlayback.Height > 90 &&
						firstPlayback.Width == firstPlayback.SourceWidth && firstPlayback.Height == firstPlayback.SourceHeight &&
						firstPlayback.CurrentTime > 0.2 && firstPlayback.Frames > 0 && firstPlayback.AudioPackets > 0 && firstPlayback.VideoPackets > 0
				})
				cameraEventually(t, browser, "continued source, protocol ingress and decoded audio/video", func() bool {
					source := cameraStream(t, srv, lab.Source)
					target := cameraStream(t, srv, streamKey)
					playback := cameraPlayback(t, browser)
					currentLab := cameraLab(t, srv, tc.protocol, streamKey)
					return currentLab.State == "active" && currentLab.Audio > 0 && currentLab.Video > 0 && currentLab.RTP > 0 && currentLab.RTCP > 0 &&
						cameraStreamAdvanced(source, firstSource) && cameraStreamAdvanced(target, firstTarget) &&
						playback.CurrentTime > firstPlayback.CurrentTime+0.5 && playback.Frames > firstPlayback.Frames &&
						playback.AudioPackets > firstPlayback.AudioPackets && playback.VideoPackets > firstPlayback.VideoPackets
				})
				ports := cameraProtocolPorts(t, srv, tc.protocol, streamKey)
				if len(ports) == 0 {
					t.Fatal("normal protocol ingress did not own RTP/RTCP ports")
				}
				for _, port := range ports {
					if port <= 0 {
						t.Fatalf("normal protocol ingress has an unbound RTP/RTCP port: %v", ports)
					}
				}
				t.Logf("cycle %d: browser %s capture reached %s as %s/%s; decoded camera %dx%d with advancing audio/video", cycle+1, lab.Source, streamKey,
					firstTarget.VideoCodec, firstTarget.AudioCodec, firstPlayback.Width, firstPlayback.Height)

				// Close only the test's receiving player; the Console must own and stop
				// its capture, temporary WHIP publisher and protocol session itself.
				cameraEvaluate(t, browser, `window.__cameraPlayback.pc.close();
fetch(window.__cameraPlayback.location, {method:'DELETE'}).catch(() => {});
window.__cameraDeletes = [];
document.getElementById('btn-stop-publish').click();`, nil)
				var stopResponses []cameraDeleteResponse
				cameraEventually(t, browser, "both protocol and WHIP Stop responses", func() bool {
					cameraEvaluate(t, browser, `window.__cameraDeletes.filter(r => r.url.includes('/lab/sessions/') || r.url === window.__cameraWhipResources.at(-1))`, &stopResponses)
					return len(stopResponses) == 2
				})
				for _, response := range stopResponses {
					t.Logf("cycle %d Stop: %s => HTTP %d %s", cycle+1, response.URL, response.Status, strings.TrimSpace(response.Body))
					// Closing the PeerConnection may retire its WHIP resource
					// before DELETE arrives. Lab Stop is explicitly idempotent
					// and must still succeed after source retirement.
					if response.Status >= 200 && response.Status < 300 {
						continue
					}
					if !strings.Contains(response.URL, "/lab/sessions/") && response.Status == http.StatusNotFound {
						continue
					}
					t.Errorf("Stop returned HTTP %d for %s: %s; protocol snapshot: %+v", response.Status, response.URL, strings.TrimSpace(response.Body), cameraLab(t, srv, tc.protocol, streamKey))
				}
				cameraEventually(t, browser, "Stop releasing capture tracks and both publishers", func() bool {
					var stopped bool
					cameraEvaluate(t, browser, `window.__cameraTracks.length >= 2 && window.__cameraTracks.every(t => t.readyState === 'ended')`, &stopped)
					return stopped && cameraLab(t, srv, tc.protocol, streamKey).State == "stopped" &&
						cameraStream(t, srv, lab.Source).Publisher == "" && cameraStream(t, srv, streamKey).Publisher == "" &&
						len(cameraProtocolPorts(t, srv, tc.protocol, streamKey)) == 0
				})
				for _, port := range ports {
					conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
					if err != nil {
						t.Fatalf("Stop leaked protocol media port %d: %v", port, err)
					}
					_ = conn.Close()
				}
			}
		})
	}
}

func cameraFields(protocol, codec, streamKey string) string {
	fields := map[string]string{"publish-sip-device": "camera-regression", "publish-sip-stream": streamKey, "publish-sip-codec": codec}
	if protocol == "gb28181" {
		fields = map[string]string{"publish-gb-device": "34020000001320000991", "publish-gb-channel": "34020000001320000992", "publish-gb-stream": streamKey}
	}
	encoded, _ := json.Marshal(fields)
	return string(encoded)
}

func cameraEvaluate(t *testing.T, browser context.Context, expression string, result any) {
	t.Helper()
	if err := chromedp.Run(browser, chromedp.Evaluate(expression, result)); err != nil {
		t.Fatalf("camera browser evaluation: %v", err)
	}
}

func cameraEventually(t *testing.T, browser context.Context, description string, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	var status string
	cameraEvaluate(t, browser, `document.getElementById('publish-status').textContent`, &status)
	t.Fatalf("timed out waiting for %s; Console status: %s", description, status)
}

func cameraGet(t *testing.T, srv *testutil.TestServer, path string, result any) {
	t.Helper()
	client := &http.Client{Timeout: time.Second}
	response, err := client.Get("http://" + srv.APIAddr() + path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		if response.StatusCode == http.StatusNotFound {
			return
		}
		t.Fatalf("read %s: HTTP %d", path, response.StatusCode)
	}
	if err := json.NewDecoder(response.Body).Decode(result); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
}

func cameraStream(t *testing.T, srv *testutil.TestServer, key string) api.StreamInfo {
	t.Helper()
	var response struct {
		Data api.StreamInfo `json:"data"`
	}
	cameraGet(t, srv, "/api/v1/streams/"+key, &response)
	return response.Data
}

func cameraStreamReady(stream api.StreamInfo, audio avframe.CodecType) bool {
	return stream.Publisher != "" && stream.VideoCodec == "H264" && stream.AudioCodec == audio.String() &&
		stream.Stats != nil && stream.Stats.VideoFrames > 0 && stream.Stats.AudioFrames > 0
}

func cameraStreamAdvanced(current, previous api.StreamInfo) bool {
	return current.Stats != nil && previous.Stats != nil && current.Publisher == previous.Publisher &&
		current.Stats.VideoFrames > previous.Stats.VideoFrames && current.Stats.AudioFrames > previous.Stats.AudioFrames
}

type cameraLabSnapshot struct {
	Source, State, Error    string
	Audio, Video, RTP, RTCP uint64
}

type cameraDeleteResponse struct {
	URL    string
	Status int
	Body   string
}

func cameraLab(t *testing.T, srv *testutil.TestServer, protocol, key string) cameraLabSnapshot {
	t.Helper()
	var latest time.Time
	var result cameraLabSnapshot
	if protocol == "sip" {
		for _, s := range srv.ModuleByName("sipgateway").(*sipgateway.Module).ListLabSessions() {
			if s.StreamKey == key && s.StartedAt.After(latest) {
				latest = s.StartedAt
				result = cameraLabSnapshot{s.SourceStreamKey, string(s.State), s.LastError, s.AudioRTPPacketsSent, s.VideoRTPPacketsSent, s.RTPPacketsSent, s.RTCPPacketsSent}
			}
		}
	} else {
		for _, s := range srv.ModuleByName("gb28181").(*gb28181.Module).ListLabSessions() {
			if s.StreamKey == key && s.StartedAt.After(latest) {
				latest = s.StartedAt
				result = cameraLabSnapshot{s.SourceStreamKey, string(s.State), s.LastError, s.AudioFramesSent, s.VideoFramesSent, s.RTPPacketsSent, s.RTCPPacketsRecv}
			}
		}
	}
	return result
}

func cameraProtocolPorts(t *testing.T, srv *testutil.TestServer, protocol, key string) []int {
	t.Helper()
	if protocol == "sip" {
		for _, call := range srv.ModuleByName("sipgateway").(*sipgateway.Module).ListCalls() {
			if call.StreamKey == key && (call.State == sipgateway.CallStateActive || call.State == sipgateway.CallStateEstablishing) {
				return []int{call.RTPPort, call.RTCPPort, call.VideoRTPPort, call.VideoRTCPPort}
			}
		}
		return nil
	}
	var sessions []struct {
		StreamKey string `json:"stream_key"`
		LocalPort int    `json:"local_port"`
	}
	cameraGet(t, srv, "/api/v1/gb28181/sessions", &sessions)
	for _, session := range sessions {
		if session.StreamKey == key {
			return []int{session.LocalPort, session.LocalPort + 1}
		}
	}
	return nil
}

type cameraPlaybackProbe struct {
	Width, Height, SourceWidth, SourceHeight int
	CurrentTime                              float64
	Frames, VideoPackets, AudioPackets       uint64
	Error                                    string
}

func cameraPlayback(t *testing.T, browser context.Context) cameraPlaybackProbe {
	t.Helper()
	var result cameraPlaybackProbe
	cameraEvaluate(t, browser, `(() => {
const p = window.__cameraPlayback, v = p.video, source = document.getElementById('publish-preview');
return {width:v.videoWidth, height:v.videoHeight, sourceWidth:source.videoWidth, sourceHeight:source.videoHeight,
  currentTime:v.currentTime, frames:p.frames, videoPackets:p.videoPackets, audioPackets:p.audioPackets,
  error:p.error || (v.error ? String(v.error.code) : '')};
})()`, &result)
	if result.Error != "" {
		t.Fatalf("protocol target browser playback failed: %s", result.Error)
	}
	return result
}

func cameraTargetPlayer(base, streamKey string) string {
	return fmt.Sprintf(`(() => {
const video = document.createElement('video');
video.autoplay = true; video.muted = true; video.playsInline = true;
document.body.appendChild(video);
const pc = new RTCPeerConnection(), media = new MediaStream();
const p = window.__cameraPlayback = {pc, video, error:'', frames:0, videoPackets:0, audioPackets:0, location:''};
pc.ontrack = event => { media.addTrack(event.track); video.srcObject = media; video.play().catch(() => {}); };
pc.addTransceiver('video', {direction:'recvonly'});
pc.addTransceiver('audio', {direction:'recvonly'});
(async () => {
  await pc.setLocalDescription(await pc.createOffer());
  await new Promise(resolve => {
    if (pc.iceGatheringState === 'complete') return resolve();
    const timer = setTimeout(resolve, 2000);
    pc.onicegatheringstatechange = () => { if (pc.iceGatheringState === 'complete') { clearTimeout(timer); resolve(); } };
  });
  const response = await fetch(%q + '/webrtc/whep/' + %q + '?mode=live', {
    method:'POST', headers:{'Content-Type':'application/sdp'}, body:pc.localDescription.sdp
  });
  if (!response.ok) throw new Error('WHEP ' + response.status + ': ' + await response.text());
  p.location = new URL(response.headers.get('Location'), %q).href;
  await pc.setRemoteDescription({type:'answer', sdp:await response.text()});
  const timer = setInterval(async () => {
    if (pc.connectionState === 'closed') { clearInterval(timer); return; }
    const stats = await pc.getStats();
    stats.forEach(r => {
      if (r.type !== 'inbound-rtp') return;
      if (r.kind === 'video') { p.videoPackets = r.packetsReceived || 0; p.frames = r.framesDecoded || 0; }
      if (r.kind === 'audio') p.audioPackets = r.packetsReceived || 0;
    });
  }, 100);
})().catch(error => { p.error = String(error); });
return true;
})()`, base, streamKey, base)
}
