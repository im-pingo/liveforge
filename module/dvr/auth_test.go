package dvr

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
)

func TestPlaybackHandlersEmitSubscribeAuthentication(t *testing.T) {
	server := core.NewServer(config.Defaults())
	var got *core.EventContext
	server.GetEventBus().Register(core.HookRegistration{
		Event: core.EventSubscribe,
		Mode:  core.HookSync,
		Handler: func(ctx *core.EventContext) error {
			copy := *ctx
			got = &copy
			return errors.New("denied")
		},
	})
	m := NewModule()
	m.server = server

	req := httptest.NewRequest(http.MethodGet, "/dvr/live/cam.m3u8?token=bad", nil)
	req.RemoteAddr = "192.0.2.10:1234"
	req.SetPathValue("app", "live")
	req.SetPathValue("key", "cam")
	w := httptest.NewRecorder()
	m.handlePlaylist(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d want=403 body=%s", w.Code, w.Body.String())
	}
	if got == nil || got.StreamKey != "live/cam" || got.Protocol != "dvr" || got.RemoteAddr != req.RemoteAddr || got.Params["token"] != "bad" {
		t.Fatalf("event context=%+v", got)
	}

	segmentReq := httptest.NewRequest(http.MethodGet, "/dvr/live/cam/seg_000001.ts?token=bad", nil)
	segmentReq.RemoteAddr = "192.0.2.11:1234"
	segmentReq.SetPathValue("app", "live")
	segmentReq.SetPathValue("key", "cam")
	segmentReq.SetPathValue("filename", "seg_000001.ts")
	segment := httptest.NewRecorder()
	m.handleSegment(segment, segmentReq)
	if segment.Code != http.StatusForbidden || got == nil || got.RemoteAddr != segmentReq.RemoteAddr || got.Params["token"] != "bad" {
		t.Fatalf("segment status=%d event context=%+v", segment.Code, got)
	}

	req = httptest.NewRequest(http.MethodGet, "/dvr/live/cam.m3u8", nil)
	req.SetPathValue("app", "live")
	req.SetPathValue("key", "cam")
	w = httptest.NewRecorder()
	m.handlePlaylist(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("missing token status=%d want=401", w.Code)
	}
}

func TestPlaylistPropagatesTokenToSegmentURLs(t *testing.T) {
	index := NewSegmentIndex()
	index.Add(Segment{SeqNum: 1, StartTime: time.Date(2026, 8, 24, 1, 2, 3, 0, time.UTC), Duration: 6, Filename: "seg_000001.ts"})
	playlist := GeneratePlaylistWithQuery(index, "live/cam", true, "token="+"a%2Bb")
	if !strings.Contains(playlist, "cam/seg_000001.ts?token=a%2Bb") {
		t.Fatalf("playlist does not propagate encoded token:\n%s", playlist)
	}
}
