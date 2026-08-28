package httpstream

import (
	"context"
	"strings"
	"testing"

	configruntime "github.com/im-pingo/liveforge/config/runtime"
	"github.com/im-pingo/liveforge/core"
)

func TestLLHLSPlaylistGenerate_BasicTags(t *testing.T) {
	p := NewLLHLSPlaylist(0.2, 6.0, "/live/test", "fmp4")

	segments := []*LLHLSSegment{
		{
			MSN:      0,
			Duration: 6.0,
			Parts: []*LLHLSPart{
				{Index: 0, Duration: 0.2, Independent: true},
				{Index: 1, Duration: 0.2, Independent: false},
			},
		},
	}
	currentParts := []*LLHLSPart{
		{Index: 0, Duration: 0.2, Independent: true},
	}

	m3u8 := p.Generate(segments, currentParts, 1, false, false)

	checks := []string{
		"#EXTM3U",
		"#EXT-X-VERSION:9",
		"#EXT-X-TARGETDURATION:6",
		"#EXT-X-PART-INF:PART-TARGET=0.200",
		"#EXT-X-SERVER-CONTROL:CAN-BLOCK-RELOAD=YES",
		"PART-HOLD-BACK=0.600",
		"#EXT-X-MAP:URI=\"/live/test/init.mp4\"",
		"#EXT-X-MEDIA-SEQUENCE:0",
		"#EXTINF:6.000,",
		"/live/test/0.m4s",
		"#EXT-X-PART:DURATION=0.20000,URI=\"/live/test/1.0.m4s\",INDEPENDENT=YES",
		"#EXT-X-PRELOAD-HINT:TYPE=PART,URI=\"/live/test/1.1.m4s\"",
	}

	for _, c := range checks {
		if !strings.Contains(m3u8, c) {
			t.Errorf("playlist missing %q\nGot:\n%s", c, m3u8)
		}
	}
	if strings.Contains(m3u8, "/live/test/0.0.m4s") || strings.Contains(m3u8, "/live/test/0.1.m4s") {
		t.Fatalf("completed segment parts were advertised alongside the full segment:\n%s", m3u8)
	}
}

func TestLLHLSPlaylistGenerate_TSContainer(t *testing.T) {
	p := NewLLHLSPlaylist(0.2, 6.0, "/live/test", "ts")

	segments := []*LLHLSSegment{
		{MSN: 0, Duration: 6.0, Parts: []*LLHLSPart{
			{Index: 0, Duration: 0.2, Independent: true},
		}},
	}

	m3u8 := p.Generate(segments, nil, 1, false, false)

	if strings.Contains(m3u8, "EXT-X-MAP") {
		t.Error("TS container should not have EXT-X-MAP")
	}
	if !strings.Contains(m3u8, "/live/test/0.ts") {
		t.Error("TS container should advertise the completed .ts segment")
	}
	if strings.Contains(m3u8, "/live/test/0.0.ts") {
		t.Error("TS playlist advertised a completed segment part alongside the full segment")
	}
}

func TestLLHLSPlaylistGenerate_DeltaUpdate(t *testing.T) {
	p := NewLLHLSPlaylist(0.2, 6.0, "/live/test", "fmp4")

	segments := make([]*LLHLSSegment, 4)
	for i := range segments {
		segments[i] = &LLHLSSegment{
			MSN:      i,
			Duration: 6.0,
			Parts:    []*LLHLSPart{{Index: 0, Duration: 0.2, Independent: true}},
		}
	}

	m3u8 := p.Generate(segments, nil, 4, true, false)

	if !strings.Contains(m3u8, "#EXT-X-SKIP:SKIPPED-SEGMENTS=2") {
		t.Errorf("expected EXT-X-SKIP with 2 skipped segments\nGot:\n%s", m3u8)
	}
	if strings.Contains(m3u8, "/live/test/0.m4s") {
		t.Error("delta update should not contain skipped segment 0")
	}
}

func TestLLHLSPlaylistGenerate_EmptySegments(t *testing.T) {
	p := NewLLHLSPlaylist(0.2, 6.0, "/live/test", "fmp4")

	m3u8 := p.Generate(nil, nil, 0, false, false)

	if !strings.Contains(m3u8, "#EXTM3U") {
		t.Error("empty playlist should still have EXTM3U header")
	}
	if !strings.Contains(m3u8, "#EXT-X-PRELOAD-HINT") {
		t.Error("empty playlist should still have PRELOAD-HINT")
	}
}

func TestLLHLSConfiguredSegmentDurationSetsEmptyPlaylistTarget(t *testing.T) {
	stream := newAudioOnlyAACStream(t, "live/llhls-playlist-target")
	cfg, err := configruntime.ParseDocument([]byte("http_stream:\n  llhls:\n    enabled: true\n    segment_duration: 1.2\n"))
	if err != nil {
		t.Fatal(err)
	}
	server := core.NewServer(cfg)
	module := NewModule()
	module.server = server
	mgr := module.getOrCreateLLHLS(stream.Key(), stream)
	t.Cleanup(func() {
		mgr.Stop()
		stream.RingBuffer().Close()
	})

	playlist, err := mgr.GeneratePlaylist(context.Background(), -1, -1, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(playlist, "#EXT-X-TARGETDURATION:2") {
		t.Fatalf("empty LL-HLS playlist ignored configured segment duration:\n%s", playlist)
	}
}

func TestLLHLSManagerVersionsInitSegmentURL(t *testing.T) {
	mgr := NewLLHLSManager("live/versioned", "/live/versioned", 0.2, 1.0, 5, "fmp4")
	mgr.segmenter.callbacks.OnInit([]byte("video and audio configuration"))

	playlist, err := mgr.GeneratePlaylist(context.Background(), -1, -1, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(playlist, `init.mp4?v=`) {
		t.Fatal("LL-HLS playlist init URL is not versioned")
	}
}

func TestLLHLSManagerRetainsLatestCompletedPartIdentityOnBlockingReload(t *testing.T) {
	mgr := NewLLHLSManager("live/reload", "/live/reload", 0.2, 1.0, 5, "fmp4")
	part := &LLHLSPart{Index: 0, Duration: 0.2, Independent: true, Data: []byte("part")}
	mgr.mu.Lock()
	mgr.segments = []*LLHLSSegment{{MSN: 0, Duration: 0.2, Parts: []*LLHLSPart{part}}}
	mgr.currentMSN = 1
	mgr.mu.Unlock()

	initial, err := mgr.GeneratePlaylist(context.Background(), -1, -1, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(initial, "/live/reload/0.0.m4s") {
		t.Fatalf("initial playlist advertised completed parts alongside the full segment:\n%s", initial)
	}

	reload, err := mgr.GeneratePlaylist(context.Background(), 0, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reload, `#EXT-X-PART:DURATION=0.20000,URI="/live/reload/0.0.m4s",INDEPENDENT=YES`) {
		t.Fatalf("blocking reload omitted the completed part identity needed to avoid reloading the full segment:\n%s", reload)
	}
}
