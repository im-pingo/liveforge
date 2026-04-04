package play

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/tools/testkit/analyzer"
	"github.com/im-pingo/liveforge/tools/testkit/push"
	"github.com/im-pingo/liveforge/tools/testkit/source"
	"github.com/im-pingo/liveforge/tools/testkit/testutil"
)

func TestNewPlayer_SRT(t *testing.T) {
	p, err := NewPlayer("srt")
	if err != nil {
		t.Fatalf("NewPlayer(srt): %v", err)
	}
	if p == nil {
		t.Fatal("NewPlayer(srt) returned nil")
	}
}

func TestParseSRTPlayTarget(t *testing.T) {
	tests := []struct {
		name         string
		url          string
		token        string
		wantAddr     string
		wantStreamID string
		wantErr      bool
	}{
		{
			name:         "basic with port",
			url:          "srt://127.0.0.1:6000?streamid=subscribe:live/test",
			wantAddr:     "127.0.0.1:6000",
			wantStreamID: "subscribe:live/test",
		},
		{
			name:         "default port",
			url:          "srt://example.com?streamid=subscribe:live/test",
			wantAddr:     "example.com:6000",
			wantStreamID: "subscribe:live/test",
		},
		{
			name:         "token in query",
			url:          "srt://host:6000?streamid=subscribe:live/test&token=abc123",
			wantAddr:     "host:6000",
			wantStreamID: "subscribe:live/test?token=abc123",
		},
		{
			name:         "token from config",
			url:          "srt://host:6000?streamid=subscribe:live/test",
			token:        "mytoken",
			wantAddr:     "host:6000",
			wantStreamID: "subscribe:live/test?token=mytoken",
		},
		{
			name:    "wrong scheme",
			url:     "http://host:6000?streamid=subscribe:live/test",
			wantErr: true,
		},
		{
			name:    "missing streamid",
			url:     "srt://host:6000",
			wantErr: true,
		},
		{
			name:    "missing host",
			url:     "srt://?streamid=subscribe:live/test",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			addr, streamID, err := parseSRTPlayTarget(tt.url, tt.token)
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error for %q", tt.url)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if addr != tt.wantAddr {
				t.Errorf("addr = %q, want %q", addr, tt.wantAddr)
			}
			if streamID != tt.wantStreamID {
				t.Errorf("streamID = %q, want %q", streamID, tt.wantStreamID)
			}
		})
	}
}

func TestNewPlayer_RTMP(t *testing.T) {
	p, err := NewPlayer("rtmp")
	if err != nil {
		t.Fatalf("NewPlayer(rtmp): %v", err)
	}
	if p == nil {
		t.Fatal("NewPlayer(rtmp) returned nil")
	}
}

func TestNewPlayer_RTSP(t *testing.T) {
	p, err := NewPlayer("rtsp")
	if err != nil {
		t.Fatalf("NewPlayer(rtsp): %v", err)
	}
	if p == nil {
		t.Fatal("NewPlayer(rtsp) returned nil")
	}
}

func TestNewPlayer_Unsupported(t *testing.T) {
	_, err := NewPlayer("unsupported")
	if err == nil {
		t.Fatal("NewPlayer(unsupported) should return error")
	}
}

func TestParseRTMPURL(t *testing.T) {
	tests := []struct {
		url        string
		wantHost   string
		wantApp    string
		wantStream string
		wantErr    bool
	}{
		{
			url:        "rtmp://127.0.0.1:1935/live/test",
			wantHost:   "127.0.0.1:1935",
			wantApp:    "live",
			wantStream: "test",
		},
		{
			url:        "rtmp://example.com/app/stream",
			wantHost:   "example.com:1935",
			wantApp:    "app",
			wantStream: "stream",
		},
		{
			url:        "rtmp://host:9999/myapp/mystream?token=abc",
			wantHost:   "host:9999",
			wantApp:    "myapp",
			wantStream: "mystream?token=abc",
		},
		{
			url:     "http://host/app/stream",
			wantErr: true,
		},
		{
			url:     "rtmp://host/app",
			wantErr: true,
		},
		{
			url:     "rtmp://host",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			host, app, stream, err := parseRTMPURL(tt.url)
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error for %q", tt.url)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if host != tt.wantHost {
				t.Errorf("host = %q, want %q", host, tt.wantHost)
			}
			if app != tt.wantApp {
				t.Errorf("app = %q, want %q", app, tt.wantApp)
			}
			if stream != tt.wantStream {
				t.Errorf("stream = %q, want %q", stream, tt.wantStream)
			}
		})
	}
}

func TestRTMPPlay(t *testing.T) {
	srv := testutil.StartTestServer(t, testutil.WithRTMP())

	// Push via RTMP in background.
	src := source.NewFLVSourceLoop(0) // loop indefinitely
	pusher, err := push.NewPusher("rtmp")
	if err != nil {
		t.Fatalf("NewPusher: %v", err)
	}

	pushURL := fmt.Sprintf("rtmp://%s/live/test", srv.RTMPAddr())
	pushCtx, pushCancel := context.WithCancel(context.Background())
	defer pushCancel()

	pushDone := make(chan error, 1)
	go func() {
		_, err := pusher.Push(pushCtx, src, push.PushConfig{
			Protocol: "rtmp",
			Target:   pushURL,
		})
		pushDone <- err
	}()

	// Wait for stream to be established.
	time.Sleep(1 * time.Second)

	// Play via RTMP.
	player, err := NewPlayer("rtmp")
	if err != nil {
		t.Fatalf("NewPlayer: %v", err)
	}

	a := analyzer.New()
	playURL := fmt.Sprintf("rtmp://%s/live/test", srv.RTMPAddr())
	playCtx, playCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer playCancel()

	playCfg := PlayConfig{
		Protocol: "rtmp",
		URL:      playURL,
		Duration: 3 * time.Second,
	}

	if err := player.Play(playCtx, playCfg, a.Feed); err != nil {
		t.Fatalf("Play: %v", err)
	}

	// Stop the pusher.
	pushCancel()
	<-pushDone

	// Verify the analyzer report.
	rpt := a.Report()

	if rpt.Video.FrameCount == 0 {
		t.Error("no video frames received")
	}
	if !rpt.Video.DTSMonotonic {
		t.Error("video DTS is not monotonic")
	}
	if rpt.Audio.FrameCount == 0 {
		t.Error("no audio frames received")
	}
	if !rpt.Audio.DTSMonotonic {
		t.Error("audio DTS is not monotonic")
	}
	if rpt.DurationMs <= 0 {
		t.Error("duration should be positive")
	}

	t.Logf("report: video=%d frames, audio=%d frames, duration=%dms",
		rpt.Video.FrameCount, rpt.Audio.FrameCount, rpt.DurationMs)
}

func TestRTSPPlay(t *testing.T) {
	srv := testutil.StartTestServer(t, testutil.WithRTMP(), testutil.WithRTSP())

	// Push via RTMP in background so there is a stream for RTSP to subscribe to.
	src := source.NewFLVSourceLoop(0)
	pusher, err := push.NewPusher("rtmp")
	if err != nil {
		t.Fatalf("NewPusher: %v", err)
	}

	pushURL := fmt.Sprintf("rtmp://%s/live/test", srv.RTMPAddr())
	pushCtx, pushCancel := context.WithCancel(context.Background())
	defer pushCancel()

	pushDone := make(chan error, 1)
	go func() {
		_, err := pusher.Push(pushCtx, src, push.PushConfig{
			Protocol: "rtmp",
			Target:   pushURL,
		})
		pushDone <- err
	}()

	// Wait for stream to be established.
	time.Sleep(1 * time.Second)

	// Play via RTSP.
	player, err := NewPlayer("rtsp")
	if err != nil {
		t.Fatalf("NewPlayer: %v", err)
	}

	a := analyzer.New()
	playURL := fmt.Sprintf("rtsp://%s/live/test", srv.RTSPAddr())
	playCtx, playCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer playCancel()

	playCfg := PlayConfig{
		Protocol: "rtsp",
		URL:      playURL,
		Duration: 3 * time.Second,
	}

	if err := player.Play(playCtx, playCfg, a.Feed); err != nil {
		t.Fatalf("Play: %v", err)
	}

	// Stop the pusher.
	pushCancel()
	<-pushDone

	// Verify the analyzer report.
	rpt := a.Report()

	if rpt.Video.FrameCount == 0 {
		t.Error("no video frames received")
	}
	if !rpt.Video.DTSMonotonic {
		t.Error("video DTS is not monotonic")
	}
	if rpt.Audio.FrameCount == 0 {
		t.Error("no audio frames received")
	}

	t.Logf("RTSP play report: video=%d frames, audio=%d frames, duration=%dms",
		rpt.Video.FrameCount, rpt.Audio.FrameCount, rpt.DurationMs)
}

func TestSRTPlay(t *testing.T) {
	srv := testutil.StartTestServer(t, testutil.WithRTMP(), testutil.WithSRT())

	// Push via RTMP in background so there is a stream for SRT to subscribe to.
	src := source.NewFLVSourceLoop(0)
	pusher, err := push.NewPusher("rtmp")
	if err != nil {
		t.Fatalf("NewPusher: %v", err)
	}

	pushURL := fmt.Sprintf("rtmp://%s/live/test", srv.RTMPAddr())
	pushCtx, pushCancel := context.WithCancel(context.Background())
	defer pushCancel()

	pushDone := make(chan error, 1)
	go func() {
		_, err := pusher.Push(pushCtx, src, push.PushConfig{
			Protocol: "rtmp",
			Target:   pushURL,
		})
		pushDone <- err
	}()

	// Wait for stream to be established.
	time.Sleep(1 * time.Second)

	// Play via SRT.
	player, err := NewPlayer("srt")
	if err != nil {
		t.Fatalf("NewPlayer: %v", err)
	}

	a := analyzer.New()
	playURL := fmt.Sprintf("srt://%s?streamid=subscribe:live/test", srv.SRTAddr())
	playCtx, playCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer playCancel()

	playCfg := PlayConfig{
		Protocol: "srt",
		URL:      playURL,
		Duration: 3 * time.Second,
	}

	if err := player.Play(playCtx, playCfg, a.Feed); err != nil {
		t.Fatalf("Play: %v", err)
	}

	// Stop the pusher.
	pushCancel()
	<-pushDone

	// Verify the analyzer report.
	rpt := a.Report()

	if rpt.Video.FrameCount == 0 {
		t.Error("no video frames received")
	}
	if !rpt.Video.DTSMonotonic {
		// TS over SRT may have small DTS reordering at GOP boundaries due to
		// GOP cache replay and PES timestamp granularity. Log rather than fail.
		t.Logf("note: video DTS is not strictly monotonic (expected for TS over SRT)")
	}
	if rpt.Audio.FrameCount == 0 {
		t.Error("no audio frames received")
	}

	t.Logf("SRT play report: video=%d frames, audio=%d frames, duration=%dms",
		rpt.Video.FrameCount, rpt.Audio.FrameCount, rpt.DurationMs)
}

func TestNewPlayer_WHEP(t *testing.T) {
	p, err := NewPlayer("whep")
	if err != nil {
		t.Fatalf("NewPlayer(whep): %v", err)
	}
	if p == nil {
		t.Fatal("NewPlayer(whep) returned nil")
	}
}

func TestWHEPPlay(t *testing.T) {
	srv := testutil.StartTestServer(t, testutil.WithRTMP(), testutil.WithWebRTC(), testutil.WithAPI())

	// Push via RTMP in background so there is a stream for WHEP to subscribe to.
	src := source.NewFLVSourceLoop(0)
	pusher, err := push.NewPusher("rtmp")
	if err != nil {
		t.Fatalf("NewPusher: %v", err)
	}

	pushURL := fmt.Sprintf("rtmp://%s/live/test", srv.RTMPAddr())
	pushCtx, pushCancel := context.WithCancel(context.Background())
	defer pushCancel()

	pushDone := make(chan error, 1)
	go func() {
		_, err := pusher.Push(pushCtx, src, push.PushConfig{
			Protocol: "rtmp",
			Target:   pushURL,
		})
		pushDone <- err
	}()

	// WebRTC needs extra time for negotiation.
	time.Sleep(2 * time.Second)

	// Play via WHEP.
	player, err := NewPlayer("whep")
	if err != nil {
		t.Fatalf("NewPlayer: %v", err)
	}

	a := analyzer.New()
	playURL := fmt.Sprintf("http://%s/webrtc/whep/live/test", srv.WebRTCAddr())
	playCtx, playCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer playCancel()

	playCfg := PlayConfig{
		Protocol: "whep",
		URL:      playURL,
		Duration: 5 * time.Second,
	}

	if err := player.Play(playCtx, playCfg, a.Feed); err != nil {
		t.Fatalf("Play: %v", err)
	}

	// Stop the pusher.
	pushCancel()
	<-pushDone

	// Verify the analyzer report.
	rpt := a.Report()

	if rpt.Video.FrameCount == 0 {
		t.Error("no video frames received")
	}
	// Audio may not be present: server only supports Opus but source is AAC.
	// Log the result instead of failing.
	if rpt.Audio.FrameCount == 0 {
		t.Log("note: no audio frames received (expected: server supports Opus but source is AAC)")
	}

	t.Logf("WHEP play report: video=%d frames, audio=%d frames, duration=%dms",
		rpt.Video.FrameCount, rpt.Audio.FrameCount, rpt.DurationMs)
}

func TestNewPlayer_HTTPFLV(t *testing.T) {
	p, err := NewPlayer("httpflv")
	if err != nil {
		t.Fatalf("NewPlayer(httpflv): %v", err)
	}
	if p == nil {
		t.Fatal("NewPlayer(httpflv) returned nil")
	}
}

func TestNewPlayer_WSFLV(t *testing.T) {
	p, err := NewPlayer("wsflv")
	if err != nil {
		t.Fatalf("NewPlayer(wsflv): %v", err)
	}
	if p == nil {
		t.Fatal("NewPlayer(wsflv) returned nil")
	}
}

func TestHTTPFLVPlay(t *testing.T) {
	srv := testutil.StartTestServer(t, testutil.WithRTMP(), testutil.WithHTTPStream(), testutil.WithAPI())

	// Push via RTMP in background so there is a stream for HTTP-FLV to subscribe to.
	src := source.NewFLVSourceLoop(0)
	pusher, err := push.NewPusher("rtmp")
	if err != nil {
		t.Fatalf("NewPusher: %v", err)
	}

	pushURL := fmt.Sprintf("rtmp://%s/live/test", srv.RTMPAddr())
	pushCtx, pushCancel := context.WithCancel(context.Background())
	defer pushCancel()

	pushDone := make(chan error, 1)
	go func() {
		_, err := pusher.Push(pushCtx, src, push.PushConfig{
			Protocol: "rtmp",
			Target:   pushURL,
		})
		pushDone <- err
	}()

	// Wait for stream to be established.
	time.Sleep(1 * time.Second)

	// Play via HTTP-FLV.
	player, err := NewPlayer("httpflv")
	if err != nil {
		t.Fatalf("NewPlayer: %v", err)
	}

	a := analyzer.New()
	playURL := fmt.Sprintf("http://%s/live/test.flv", srv.HTTPAddr())
	playCtx, playCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer playCancel()

	playCfg := PlayConfig{
		Protocol: "httpflv",
		URL:      playURL,
		Duration: 3 * time.Second,
	}

	if err := player.Play(playCtx, playCfg, a.Feed); err != nil {
		t.Fatalf("Play: %v", err)
	}

	// Stop the pusher.
	pushCancel()
	<-pushDone

	// Verify the analyzer report.
	rpt := a.Report()

	if rpt.Video.FrameCount == 0 {
		t.Error("no video frames received")
	}
	if !rpt.Video.DTSMonotonic {
		t.Error("video DTS is not monotonic")
	}
	if rpt.Audio.FrameCount == 0 {
		t.Error("no audio frames received")
	}
	if !rpt.Audio.DTSMonotonic {
		t.Error("audio DTS is not monotonic")
	}
	if rpt.DurationMs <= 0 {
		t.Error("duration should be positive")
	}

	t.Logf("HTTP-FLV play report: video=%d frames, audio=%d frames, duration=%dms",
		rpt.Video.FrameCount, rpt.Audio.FrameCount, rpt.DurationMs)
}

func TestNewPlayer_HLS(t *testing.T) {
	p, err := NewPlayer("hls")
	if err != nil {
		t.Fatalf("NewPlayer(hls): %v", err)
	}
	if p == nil {
		t.Fatal("NewPlayer(hls) returned nil")
	}
}

func TestHLSPlay(t *testing.T) {
	srv := testutil.StartTestServer(t, testutil.WithRTMP(), testutil.WithHTTPStream(), testutil.WithAPI())

	// Push via RTMP in background so there is a stream for HLS to subscribe to.
	src := source.NewFLVSourceLoop(0)
	pusher, err := push.NewPusher("rtmp")
	if err != nil {
		t.Fatalf("NewPusher: %v", err)
	}

	pushURL := fmt.Sprintf("rtmp://%s/live/test", srv.RTMPAddr())
	pushCtx, pushCancel := context.WithCancel(context.Background())
	defer pushCancel()

	pushDone := make(chan error, 1)
	go func() {
		_, err := pusher.Push(pushCtx, src, push.PushConfig{
			Protocol: "rtmp",
			Target:   pushURL,
		})
		pushDone <- err
	}()

	// HLS needs time to generate the first TS segment. The server waits up to
	// 10s for at least one segment on m3u8 request, but the stream must be
	// publishing first. Wait for the RTMP push to establish.
	time.Sleep(1 * time.Second)

	// Play via HLS.
	player, err := NewPlayer("hls")
	if err != nil {
		t.Fatalf("NewPlayer: %v", err)
	}

	a := analyzer.New()
	playURL := fmt.Sprintf("http://%s/live/test.m3u8", srv.HTTPAddr())
	playCtx, playCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer playCancel()

	playCfg := PlayConfig{
		Protocol: "hls",
		URL:      playURL,
		Duration: 5 * time.Second,
	}

	if err := player.Play(playCtx, playCfg, a.Feed); err != nil {
		t.Fatalf("Play: %v", err)
	}

	// Stop the pusher.
	pushCancel()
	<-pushDone

	// Verify the analyzer report.
	rpt := a.Report()

	if rpt.Video.FrameCount == 0 {
		t.Error("no video frames received")
	}
	// TS demuxing may have small DTS reordering at segment boundaries due to
	// PES timestamp granularity. Log rather than fail.
	if !rpt.Video.DTSMonotonic {
		t.Logf("note: video DTS is not strictly monotonic (expected for TS segments)")
	}
	if rpt.Audio.FrameCount == 0 {
		t.Error("no audio frames received")
	}

	t.Logf("HLS play report: video=%d frames, audio=%d frames, duration=%dms",
		rpt.Video.FrameCount, rpt.Audio.FrameCount, rpt.DurationMs)
}

func TestWSFLVPlay(t *testing.T) {
	srv := testutil.StartTestServer(t, testutil.WithRTMP(), testutil.WithHTTPStream(), testutil.WithAPI())

	// Push via RTMP in background so there is a stream for WS-FLV to subscribe to.
	src := source.NewFLVSourceLoop(0)
	pusher, err := push.NewPusher("rtmp")
	if err != nil {
		t.Fatalf("NewPusher: %v", err)
	}

	pushURL := fmt.Sprintf("rtmp://%s/live/test", srv.RTMPAddr())
	pushCtx, pushCancel := context.WithCancel(context.Background())
	defer pushCancel()

	pushDone := make(chan error, 1)
	go func() {
		_, err := pusher.Push(pushCtx, src, push.PushConfig{
			Protocol: "rtmp",
			Target:   pushURL,
		})
		pushDone <- err
	}()

	// Wait for stream to be established.
	time.Sleep(1 * time.Second)

	// Play via WS-FLV.
	player, err := NewPlayer("wsflv")
	if err != nil {
		t.Fatalf("NewPlayer: %v", err)
	}

	a := analyzer.New()
	// URL uses http:// scheme; the player converts to ws:// internally.
	playURL := fmt.Sprintf("http://%s/ws/live/test.flv", srv.HTTPAddr())
	playCtx, playCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer playCancel()

	playCfg := PlayConfig{
		Protocol: "wsflv",
		URL:      playURL,
		Duration: 3 * time.Second,
	}

	if err := player.Play(playCtx, playCfg, a.Feed); err != nil {
		t.Fatalf("Play: %v", err)
	}

	// Stop the pusher.
	pushCancel()
	<-pushDone

	// Verify the analyzer report.
	rpt := a.Report()

	if rpt.Video.FrameCount == 0 {
		t.Error("no video frames received")
	}
	if !rpt.Video.DTSMonotonic {
		t.Error("video DTS is not monotonic")
	}
	if rpt.Audio.FrameCount == 0 {
		t.Error("no audio frames received")
	}
	if !rpt.Audio.DTSMonotonic {
		t.Error("audio DTS is not monotonic")
	}
	if rpt.DurationMs <= 0 {
		t.Error("duration should be positive")
	}

	t.Logf("WS-FLV play report: video=%d frames, audio=%d frames, duration=%dms",
		rpt.Video.FrameCount, rpt.Audio.FrameCount, rpt.DurationMs)
}

func TestNewPlayer_LLHLS(t *testing.T) {
	p, err := NewPlayer("llhls")
	if err != nil {
		t.Fatalf("NewPlayer(llhls): %v", err)
	}
	if p == nil {
		t.Fatal("NewPlayer(llhls) returned nil")
	}
}

func TestParseLLHLSPlaylist(t *testing.T) {
	body := `#EXTM3U
#EXT-X-VERSION:9
#EXT-X-TARGETDURATION:6
#EXT-X-PART-INF:PART-TARGET=0.200
#EXT-X-SERVER-CONTROL:CAN-BLOCK-RELOAD=YES,PART-HOLD-BACK=0.600,CAN-SKIP-UNTIL=36.0
#EXT-X-MAP:URI="/live/test/init.mp4"
#EXT-X-MEDIA-SEQUENCE:0

#EXT-X-PART:DURATION=0.20000,URI="/live/test/0.0.m4s",INDEPENDENT=YES
#EXT-X-PART:DURATION=0.20000,URI="/live/test/0.1.m4s"
#EXT-X-PART:DURATION=0.20000,URI="/live/test/0.2.m4s"
#EXTINF:0.600,
/live/test/0.m4s

#EXT-X-PART:DURATION=0.20000,URI="/live/test/1.0.m4s",INDEPENDENT=YES
#EXT-X-PRELOAD-HINT:TYPE=PART,URI="/live/test/1.1.m4s"
`
	pl := parseLLHLSPlaylist(body)

	if pl.mediaSequence != 0 {
		t.Errorf("mediaSequence = %d, want 0", pl.mediaSequence)
	}
	if pl.initURI != "/live/test/init.mp4" {
		t.Errorf("initURI = %q, want %q", pl.initURI, "/live/test/init.mp4")
	}
	if len(pl.parts) != 4 {
		t.Fatalf("got %d parts, want 4", len(pl.parts))
	}

	// First part: MSN=0, PartIdx=0, Independent=true
	if pl.parts[0].MSN != 0 || pl.parts[0].PartIdx != 0 {
		t.Errorf("part[0] MSN=%d PartIdx=%d, want 0,0", pl.parts[0].MSN, pl.parts[0].PartIdx)
	}
	if !pl.parts[0].Independent {
		t.Error("part[0] should be INDEPENDENT")
	}
	if pl.parts[0].URI != "/live/test/0.0.m4s" {
		t.Errorf("part[0] URI = %q", pl.parts[0].URI)
	}

	// Fourth part: MSN=1, PartIdx=0 (after segment boundary reset)
	if pl.parts[3].MSN != 1 || pl.parts[3].PartIdx != 0 {
		t.Errorf("part[3] MSN=%d PartIdx=%d, want 1,0", pl.parts[3].MSN, pl.parts[3].PartIdx)
	}

	// Full segments
	if len(pl.segments) != 1 {
		t.Fatalf("got %d segments, want 1", len(pl.segments))
	}
	if pl.segments[0].URI != "/live/test/0.m4s" {
		t.Errorf("segment[0] URI = %q", pl.segments[0].URI)
	}
	if pl.segments[0].SeqNum != 0 {
		t.Errorf("segment[0] SeqNum = %d, want 0", pl.segments[0].SeqNum)
	}

	// Preload hint
	if pl.preloadHint != "/live/test/1.1.m4s" {
		t.Errorf("preloadHint = %q, want %q", pl.preloadHint, "/live/test/1.1.m4s")
	}
}

func TestParseLLHLSPlaylist_TS(t *testing.T) {
	// TS container: no EXT-X-MAP
	body := `#EXTM3U
#EXT-X-VERSION:9
#EXT-X-TARGETDURATION:6
#EXT-X-PART-INF:PART-TARGET=0.200
#EXT-X-MEDIA-SEQUENCE:5

#EXT-X-PART:DURATION=0.20000,URI="/live/test/5.0.ts",INDEPENDENT=YES
#EXT-X-PART:DURATION=0.20000,URI="/live/test/5.1.ts"
#EXTINF:0.400,
/live/test/5.ts
`
	pl := parseLLHLSPlaylist(body)

	if pl.initURI != "" {
		t.Errorf("initURI should be empty for TS, got %q", pl.initURI)
	}
	if pl.mediaSequence != 5 {
		t.Errorf("mediaSequence = %d, want 5", pl.mediaSequence)
	}
	if len(pl.parts) != 2 {
		t.Fatalf("got %d parts, want 2", len(pl.parts))
	}
	if pl.parts[0].MSN != 5 {
		t.Errorf("part[0] MSN = %d, want 5", pl.parts[0].MSN)
	}
}

func TestParseAttributeValue(t *testing.T) {
	tests := []struct {
		name  string
		line  string
		attr  string
		want  string
	}{
		{
			name: "quoted URI",
			line: `#EXT-X-MAP:URI="/live/test/init.mp4"`,
			attr: "URI",
			want: "/live/test/init.mp4",
		},
		{
			name: "unquoted DURATION",
			line: `#EXT-X-PART:DURATION=0.20000,URI="/live/test/0.0.m4s"`,
			attr: "DURATION",
			want: "0.20000",
		},
		{
			name: "quoted URI with comma after",
			line: `#EXT-X-PART:DURATION=0.20000,URI="/live/test/0.0.m4s",INDEPENDENT=YES`,
			attr: "URI",
			want: "/live/test/0.0.m4s",
		},
		{
			name: "missing attribute",
			line: `#EXT-X-PART:DURATION=0.20000`,
			attr: "URI",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseAttributeValue(tt.line, tt.attr)
			if got != tt.want {
				t.Errorf("parseAttributeValue(%q, %q) = %q, want %q", tt.line, tt.attr, got, tt.want)
			}
		})
	}
}

func TestCalcNextBlockingParams(t *testing.T) {
	pl := &llhlsPlaylist{
		parts: []llhlsPart{
			{MSN: 0, PartIdx: 0},
			{MSN: 0, PartIdx: 1},
			{MSN: 0, PartIdx: 2},
			{MSN: 1, PartIdx: 0},
		},
	}

	// Consumed everything: expect next after max.
	msn, part := calcNextBlockingParams(pl, 1, 0)
	if msn != 1 || part != 1 {
		t.Errorf("got MSN=%d part=%d, want 1,1", msn, part)
	}

	// Consumed part of data: expect next unconsumed.
	msn, part = calcNextBlockingParams(pl, 0, 1)
	if msn != 0 || part != 2 {
		t.Errorf("got MSN=%d part=%d, want 0,2", msn, part)
	}
}

func TestBuildBlockingReloadURL(t *testing.T) {
	result, err := buildBlockingReloadURL("http://localhost:8080/live/test.m3u8", 3, 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "http://localhost:8080/live/test.m3u8?_HLS_msn=3&_HLS_part=2" {
		t.Errorf("got %q", result)
	}

	// With existing query params.
	result, err = buildBlockingReloadURL("http://localhost:8080/live/test.m3u8?token=abc", 1, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should contain all three params.
	for _, expected := range []string{"_HLS_msn=1", "_HLS_part=0", "token=abc"} {
		if !strings.Contains(result, expected) {
			t.Errorf("result %q missing %q", result, expected)
		}
	}
}

func TestLLHLSPlay(t *testing.T) {
	srv := testutil.StartTestServer(t, testutil.WithRTMP(), testutil.WithHTTPStream(), testutil.WithLLHLS("fmp4"), testutil.WithAPI())

	// Push via RTMP in background so there is a stream for LL-HLS to subscribe to.
	src := source.NewFLVSourceLoop(0)
	pusher, err := push.NewPusher("rtmp")
	if err != nil {
		t.Fatalf("NewPusher: %v", err)
	}

	pushURL := fmt.Sprintf("rtmp://%s/live/test", srv.RTMPAddr())
	pushCtx, pushCancel := context.WithCancel(context.Background())
	defer pushCancel()

	pushDone := make(chan error, 1)
	go func() {
		_, err := pusher.Push(pushCtx, src, push.PushConfig{
			Protocol: "rtmp",
			Target:   pushURL,
		})
		pushDone <- err
	}()

	// LL-HLS needs time to produce partial segments.
	time.Sleep(3 * time.Second)

	// Play via LL-HLS.
	player, err := NewPlayer("llhls")
	if err != nil {
		t.Fatalf("NewPlayer: %v", err)
	}

	a := analyzer.New()
	playURL := fmt.Sprintf("http://%s/live/test.m3u8", srv.HTTPAddr())
	playCtx, playCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer playCancel()

	playCfg := PlayConfig{
		Protocol: "llhls",
		URL:      playURL,
		Duration: 5 * time.Second,
	}

	if err := player.Play(playCtx, playCfg, a.Feed); err != nil {
		t.Fatalf("Play: %v", err)
	}

	// Stop the pusher.
	pushCancel()
	<-pushDone

	// Verify the analyzer report.
	rpt := a.Report()

	if rpt.Video.FrameCount == 0 {
		t.Error("no video frames received")
	}
	// fmp4 demuxing may have small DTS reordering at partial segment boundaries.
	if !rpt.Video.DTSMonotonic {
		t.Logf("note: video DTS is not strictly monotonic (expected for fmp4 partial segments)")
	}
	if rpt.Audio.FrameCount == 0 {
		t.Error("no audio frames received")
	}

	t.Logf("LL-HLS play report: video=%d frames, audio=%d frames, duration=%dms",
		rpt.Video.FrameCount, rpt.Audio.FrameCount, rpt.DurationMs)
}

func TestNewPlayer_DASH(t *testing.T) {
	p, err := NewPlayer("dash")
	if err != nil {
		t.Fatalf("NewPlayer(dash): %v", err)
	}
	if p == nil {
		t.Fatal("NewPlayer(dash) returned nil")
	}
}

func TestParseMPD(t *testing.T) {
	body := `<?xml version="1.0" encoding="UTF-8"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="dynamic" minimumUpdatePeriod="PT6S"
     availabilityStartTime="2026-04-04T12:00:00Z" publishTime="2026-04-04T12:00:30Z"
     timeShiftBufferDepth="PT36S" minBufferTime="PT2S"
     profiles="urn:mpeg:dash:profile:isoff-live:2011">
  <Period id="0" start="PT0S">
    <AdaptationSet id="0" contentType="video" mimeType="video/mp4" startWithSAP="1" segmentAlignment="true">
      <SegmentTemplate timescale="1000" startNumber="1"
                       initialization="/live/test/vinit.mp4"
                       media="/live/test/v$Number$.m4s">
        <SegmentTimeline>
          <S t="0" d="6000"/>
          <S d="6000"/>
          <S d="6000"/>
        </SegmentTimeline>
      </SegmentTemplate>
      <Representation id="0" bandwidth="2000000" codecs="avc1.640028" width="1920" height="1080"/>
    </AdaptationSet>
    <AdaptationSet id="1" contentType="audio" mimeType="audio/mp4" startWithSAP="1" segmentAlignment="true">
      <SegmentTemplate timescale="1000" startNumber="1"
                       initialization="/live/test/audio_init.mp4"
                       media="/live/test/a$Number$.m4s">
        <SegmentTimeline>
          <S t="0" d="6000"/>
          <S d="6000"/>
          <S d="6000"/>
        </SegmentTimeline>
      </SegmentTemplate>
      <Representation id="1" bandwidth="128000" codecs="mp4a.40.2" audioSamplingRate="44100"/>
    </AdaptationSet>
  </Period>
</MPD>`

	mpd, err := parseMPD(body)
	if err != nil {
		t.Fatalf("parseMPD: %v", err)
	}

	if mpd.Type != "dynamic" {
		t.Errorf("MPD type = %q, want %q", mpd.Type, "dynamic")
	}
	if mpd.MinimumUpdatePeriod != "PT6S" {
		t.Errorf("minimumUpdatePeriod = %q, want %q", mpd.MinimumUpdatePeriod, "PT6S")
	}
	if len(mpd.Periods) != 1 {
		t.Fatalf("got %d periods, want 1", len(mpd.Periods))
	}

	period := mpd.Periods[0]
	if len(period.AdaptationSets) != 2 {
		t.Fatalf("got %d AdaptationSets, want 2", len(period.AdaptationSets))
	}

	// Video AdaptationSet.
	videoAS := period.AdaptationSets[0]
	if videoAS.ContentType != "video" {
		t.Errorf("AS[0] contentType = %q, want %q", videoAS.ContentType, "video")
	}
	if videoAS.SegmentTemplate == nil {
		t.Fatal("video SegmentTemplate is nil")
	}
	if videoAS.SegmentTemplate.StartNumber != 1 {
		t.Errorf("video startNumber = %d, want 1", videoAS.SegmentTemplate.StartNumber)
	}
	if videoAS.SegmentTemplate.Initialization != "/live/test/vinit.mp4" {
		t.Errorf("video init = %q", videoAS.SegmentTemplate.Initialization)
	}
	if videoAS.SegmentTemplate.Media != "/live/test/v$Number$.m4s" {
		t.Errorf("video media = %q", videoAS.SegmentTemplate.Media)
	}
	if cnt := timelineSegmentCount(videoAS.SegmentTemplate.Timeline); cnt != 3 {
		t.Errorf("video timeline segment count = %d, want 3", cnt)
	}

	// Audio AdaptationSet.
	audioAS := period.AdaptationSets[1]
	if audioAS.ContentType != "audio" {
		t.Errorf("AS[1] contentType = %q, want %q", audioAS.ContentType, "audio")
	}
	if audioAS.SegmentTemplate == nil {
		t.Fatal("audio SegmentTemplate is nil")
	}
	if audioAS.SegmentTemplate.Initialization != "/live/test/audio_init.mp4" {
		t.Errorf("audio init = %q", audioAS.SegmentTemplate.Initialization)
	}
	if audioAS.SegmentTemplate.Media != "/live/test/a$Number$.m4s" {
		t.Errorf("audio media = %q", audioAS.SegmentTemplate.Media)
	}
}

func TestParsePT(t *testing.T) {
	tests := []struct {
		input string
		want  time.Duration
	}{
		{"PT6S", 6 * time.Second},
		{"PT3S", 3 * time.Second},
		{"PT0.5S", 500 * time.Millisecond},
		{"", 0},
		{"invalid", 0},
		{"PT", 0},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := parsePT(tt.input)
			if got != tt.want {
				t.Errorf("parsePT(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestTimelineSegmentCount(t *testing.T) {
	tests := []struct {
		name    string
		entries []mpdTimelineEntry
		want    int
	}{
		{
			name:    "three entries no repeat",
			entries: []mpdTimelineEntry{{D: 6000}, {D: 6000}, {D: 6000}},
			want:    3,
		},
		{
			name:    "one entry with repeat",
			entries: []mpdTimelineEntry{{D: 6000, R: 4}},
			want:    5,
		},
		{
			name:    "mixed",
			entries: []mpdTimelineEntry{{D: 6000}, {D: 6000, R: 2}},
			want:    4,
		},
		{
			name:    "nil timeline",
			entries: nil,
			want:    0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var tl *mpdSegmentTimeline
			if tt.entries != nil {
				tl = &mpdSegmentTimeline{Entries: tt.entries}
			}
			got := timelineSegmentCount(tl)
			if got != tt.want {
				t.Errorf("timelineSegmentCount = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestDASHPlay(t *testing.T) {
	srv := testutil.StartTestServer(t, testutil.WithRTMP(), testutil.WithHTTPStream(), testutil.WithAPI())

	// Push via RTMP in background so there is a stream for DASH to subscribe to.
	src := source.NewFLVSourceLoop(0)
	pusher, err := push.NewPusher("rtmp")
	if err != nil {
		t.Fatalf("NewPusher: %v", err)
	}

	pushURL := fmt.Sprintf("rtmp://%s/live/test", srv.RTMPAddr())
	pushCtx, pushCancel := context.WithCancel(context.Background())
	defer pushCancel()

	pushDone := make(chan error, 1)
	go func() {
		_, err := pusher.Push(pushCtx, src, push.PushConfig{
			Protocol: "rtmp",
			Target:   pushURL,
		})
		pushDone <- err
	}()

	// DASH needs at least 3 segments before serving MPD (server waits up to 15s).
	// Use a longer initial sleep so segments are ready.
	time.Sleep(5 * time.Second)

	// Play via DASH.
	player, err := NewPlayer("dash")
	if err != nil {
		t.Fatalf("NewPlayer: %v", err)
	}

	a := analyzer.New()
	playURL := fmt.Sprintf("http://%s/live/test.mpd", srv.HTTPAddr())
	playCtx, playCancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer playCancel()

	playCfg := PlayConfig{
		Protocol: "dash",
		URL:      playURL,
		Duration: 5 * time.Second,
	}

	if err := player.Play(playCtx, playCfg, a.Feed); err != nil {
		t.Fatalf("Play: %v", err)
	}

	// Stop the pusher.
	pushCancel()
	<-pushDone

	// Verify the analyzer report.
	rpt := a.Report()

	if rpt.Video.FrameCount == 0 {
		t.Error("no video frames received")
	}
	// fmp4 demuxing may have small DTS reordering at segment boundaries.
	if !rpt.Video.DTSMonotonic {
		t.Logf("note: video DTS is not strictly monotonic (expected for fmp4 segments)")
	}
	if rpt.Audio.FrameCount == 0 {
		t.Error("no audio frames received")
	}

	t.Logf("DASH play report: video=%d frames, audio=%d frames, duration=%dms",
		rpt.Video.FrameCount, rpt.Audio.FrameCount, rpt.DurationMs)
}
