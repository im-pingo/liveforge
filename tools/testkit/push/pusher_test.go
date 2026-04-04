package push

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/tools/testkit/source"
	"github.com/im-pingo/liveforge/tools/testkit/testutil"
)

func TestNewPusher_RTMP(t *testing.T) {
	p, err := NewPusher("rtmp")
	if err != nil {
		t.Fatalf("NewPusher(rtmp): %v", err)
	}
	if p == nil {
		t.Fatal("NewPusher(rtmp) returned nil")
	}
}

func TestNewPusher_Unsupported(t *testing.T) {
	_, err := NewPusher("unsupported")
	if err == nil {
		t.Fatal("NewPusher(unsupported) should return error")
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

func TestRTMPPush(t *testing.T) {
	srv := testutil.StartTestServer(t, testutil.WithRTMP())

	src := source.NewFLVSourceLoop(2)
	p, err := NewPusher("rtmp")
	if err != nil {
		t.Fatalf("NewPusher: %v", err)
	}

	cfg := PushConfig{
		Protocol: "rtmp",
		Target:   fmt.Sprintf("rtmp://%s/live/test", srv.RTMPAddr()),
		Duration: 3 * time.Second,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rpt, err := p.Push(ctx, src, cfg)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if rpt.FramesSent == 0 {
		t.Error("no frames sent")
	}
	if rpt.BytesSent == 0 {
		t.Error("no bytes sent")
	}
	if rpt.Protocol != "rtmp" {
		t.Errorf("protocol = %q, want %q", rpt.Protocol, "rtmp")
	}
	if rpt.DurationMs <= 0 {
		t.Error("duration should be positive")
	}
}

func TestNewPusher_RTSP(t *testing.T) {
	p, err := NewPusher("rtsp")
	if err != nil {
		t.Fatalf("NewPusher(rtsp): %v", err)
	}
	if p == nil {
		t.Fatal("NewPusher(rtsp) returned nil")
	}
}

func TestRTSPPush(t *testing.T) {
	srv := testutil.StartTestServer(t, testutil.WithRTSP())

	src := source.NewFLVSourceLoop(2)
	p, err := NewPusher("rtsp")
	if err != nil {
		t.Fatalf("NewPusher: %v", err)
	}

	cfg := PushConfig{
		Protocol: "rtsp",
		Target:   fmt.Sprintf("rtsp://%s/live/test", srv.RTSPAddr()),
		Duration: 3 * time.Second,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rpt, err := p.Push(ctx, src, cfg)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if rpt.FramesSent == 0 {
		t.Error("no frames sent")
	}
	if rpt.BytesSent == 0 {
		t.Error("no bytes sent")
	}
	if rpt.Protocol != "rtsp" {
		t.Errorf("protocol = %q, want %q", rpt.Protocol, "rtsp")
	}
	if rpt.DurationMs <= 0 {
		t.Error("duration should be positive")
	}
}

func TestNewPusher_SRT(t *testing.T) {
	p, err := NewPusher("srt")
	if err != nil {
		t.Fatalf("NewPusher(srt): %v", err)
	}
	if p == nil {
		t.Fatal("NewPusher(srt) returned nil")
	}
}

func TestParseSRTTarget(t *testing.T) {
	tests := []struct {
		url          string
		wantAddr     string
		wantStreamID string
		wantErr      bool
	}{
		{
			url:          "srt://127.0.0.1:6000?streamid=publish:live/test",
			wantAddr:     "127.0.0.1:6000",
			wantStreamID: "publish:live/test",
		},
		{
			url:          "srt://example.com?streamid=publish:live/stream",
			wantAddr:     "example.com:6000",
			wantStreamID: "publish:live/stream",
		},
		{
			url:          "srt://host:9000?streamid=publish:live/test&token=secret123",
			wantAddr:     "host:9000",
			wantStreamID: "publish:live/test?token=secret123",
		},
		{
			url:     "rtmp://host:1935?streamid=publish:live/test",
			wantErr: true,
		},
		{
			url:     "srt://host:6000",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			addr, streamID, err := parseSRTTarget(tt.url)
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

func TestSRTPush(t *testing.T) {
	srv := testutil.StartTestServer(t, testutil.WithSRT())

	src := source.NewFLVSourceLoop(2)
	p, err := NewPusher("srt")
	if err != nil {
		t.Fatalf("NewPusher: %v", err)
	}

	cfg := PushConfig{
		Protocol: "srt",
		Target:   fmt.Sprintf("srt://%s?streamid=publish:live/test", srv.SRTAddr()),
		Duration: 3 * time.Second,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rpt, err := p.Push(ctx, src, cfg)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if rpt.FramesSent == 0 {
		t.Error("no frames sent")
	}
	if rpt.BytesSent == 0 {
		t.Error("no bytes sent")
	}
	if rpt.Protocol != "srt" {
		t.Errorf("protocol = %q, want %q", rpt.Protocol, "srt")
	}
	if rpt.DurationMs <= 0 {
		t.Error("duration should be positive")
	}
}

func TestRTMPPush_ContextCancel(t *testing.T) {
	srv := testutil.StartTestServer(t, testutil.WithRTMP())

	// Use infinite looping source so cancellation is the only stop condition.
	src := source.NewFLVSourceLoop(0)
	p, err := NewPusher("rtmp")
	if err != nil {
		t.Fatalf("NewPusher: %v", err)
	}

	cfg := PushConfig{
		Protocol: "rtmp",
		Target:   fmt.Sprintf("rtmp://%s/live/cancel_test", srv.RTMPAddr()),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	rpt, err := p.Push(ctx, src, cfg)
	if err == nil {
		t.Log("Push completed without error (source may have been fast enough)")
	}
	if rpt == nil {
		t.Fatal("report should not be nil even on cancellation")
	}
	if rpt.FramesSent == 0 {
		t.Error("expected some frames sent before cancellation")
	}
}
