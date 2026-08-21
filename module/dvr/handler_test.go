package dvr

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
)

type dvrAuthorizerFunc func(context.Context, core.AuthorizationRequest) error

func (f dvrAuthorizerFunc) Authorize(ctx context.Context, req core.AuthorizationRequest) error {
	return f(ctx, req)
}

func TestDVRPlaylistAuthorizationRejectsBeforeMediaRead(t *testing.T) {
	cfg := &config.Config{Stream: config.StreamConfig{RingBufferSize: 16}}
	server := core.NewServer(cfg)
	server.SetAuthorizer(dvrAuthorizerFunc(func(_ context.Context, req core.AuthorizationRequest) error {
		if req.Action != core.AuthorizationSubscribe || req.Protocol != "dvr" {
			t.Fatalf("unexpected authorization request: %+v", req)
		}
		return fmt.Errorf("denied")
	}))
	index := NewSegmentIndex()
	index.Add(Segment{SeqNum: 1, Filename: "seg_000001.ts", Duration: 1, StartTime: time.Now()})
	m := &Module{
		server:   server,
		sessions: map[string]*Session{"live/test": {index: index}},
	}
	req := httptest.NewRequest("GET", "/dvr/live/test.m3u8?token=bad", nil)
	req.SetPathValue("app", "live")
	req.SetPathValue("key", "test")
	resp := httptest.NewRecorder()

	m.handlePlaylist(resp, req)

	if resp.Code != 401 {
		t.Fatalf("status = %d, want 401", resp.Code)
	}
	if strings.Contains(resp.Body.String(), "#EXTM3U") {
		t.Fatal("rejected playlist returned media content")
	}
}

func TestDVRSegmentAuthorizationRejectsBeforeFileRead(t *testing.T) {
	cfg := &config.Config{Stream: config.StreamConfig{RingBufferSize: 16}}
	server := core.NewServer(cfg)
	server.SetAuthorizer(dvrAuthorizerFunc(func(_ context.Context, req core.AuthorizationRequest) error {
		return fmt.Errorf("denied at %s", req.Stage)
	}))
	index := NewSegmentIndex()
	index.Add(Segment{SeqNum: 1, Filename: "seg_000001.ts", Duration: 1, StartTime: time.Now()})
	m := &Module{
		server:   server,
		sessions: map[string]*Session{"live/test": {index: index}},
	}
	req := httptest.NewRequest("GET", "/dvr/live/test/seg_000001.ts?token=bad", nil)
	req.SetPathValue("app", "live")
	req.SetPathValue("key", "test")
	req.SetPathValue("filename", "seg_000001.ts")
	resp := httptest.NewRecorder()

	m.handleSegment(resp, req)

	if resp.Code != 401 {
		t.Fatalf("status = %d, want 401", resp.Code)
	}
}
