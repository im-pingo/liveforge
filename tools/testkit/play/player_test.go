package play

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/tools/testkit/analyzer"
	"github.com/im-pingo/liveforge/tools/testkit/push"
	"github.com/im-pingo/liveforge/tools/testkit/source"
	"github.com/im-pingo/liveforge/tools/testkit/testutil"
)

func TestNewPlayer_RTMP(t *testing.T) {
	p, err := NewPlayer("rtmp")
	if err != nil {
		t.Fatalf("NewPlayer(rtmp): %v", err)
	}
	if p == nil {
		t.Fatal("NewPlayer(rtmp) returned nil")
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
