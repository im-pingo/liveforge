package httpstream

import (
	"strings"
	"testing"
)

func TestLLHLSPlaylistGenerate_BasicTags(t *testing.T) {
	p := NewLLHLSPlaylist(0.2, "/live/test", "fmp4")

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

	m3u8 := p.Generate(segments, currentParts, 1, false)

	checks := []string{
		"#EXTM3U",
		"#EXT-X-VERSION:9",
		"#EXT-X-TARGETDURATION:6",
		"#EXT-X-PART-INF:PART-TARGET=0.200",
		"#EXT-X-SERVER-CONTROL:CAN-BLOCK-RELOAD=YES",
		"PART-HOLD-BACK=0.600",
		"#EXT-X-MAP:URI=\"/live/test/init.mp4\"",
		"#EXT-X-MEDIA-SEQUENCE:0",
		"#EXT-X-PART:DURATION=0.20000,URI=\"/live/test/0.0.m4s\",INDEPENDENT=YES",
		"#EXT-X-PART:DURATION=0.20000,URI=\"/live/test/0.1.m4s\"",
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
}

func TestLLHLSPlaylistGenerate_TSContainer(t *testing.T) {
	p := NewLLHLSPlaylist(0.2, "/live/test", "ts")

	segments := []*LLHLSSegment{
		{MSN: 0, Duration: 6.0, Parts: []*LLHLSPart{
			{Index: 0, Duration: 0.2, Independent: true},
		}},
	}

	m3u8 := p.Generate(segments, nil, 1, false)

	if strings.Contains(m3u8, "EXT-X-MAP") {
		t.Error("TS container should not have EXT-X-MAP")
	}
	if !strings.Contains(m3u8, "0.0.ts") {
		t.Error("TS container should use .ts extension")
	}
}

func TestLLHLSPlaylistGenerate_DeltaUpdate(t *testing.T) {
	p := NewLLHLSPlaylist(0.2, "/live/test", "fmp4")

	segments := make([]*LLHLSSegment, 4)
	for i := range segments {
		segments[i] = &LLHLSSegment{
			MSN:      i,
			Duration: 6.0,
			Parts:    []*LLHLSPart{{Index: 0, Duration: 0.2, Independent: true}},
		}
	}

	m3u8 := p.Generate(segments, nil, 4, true)

	if !strings.Contains(m3u8, "#EXT-X-SKIP:SKIPPED-SEGMENTS=2") {
		t.Errorf("expected EXT-X-SKIP with 2 skipped segments\nGot:\n%s", m3u8)
	}
	if strings.Contains(m3u8, "/live/test/0.m4s") {
		t.Error("delta update should not contain skipped segment 0")
	}
}

func TestLLHLSPlaylistGenerate_EmptySegments(t *testing.T) {
	p := NewLLHLSPlaylist(0.2, "/live/test", "fmp4")

	m3u8 := p.Generate(nil, nil, 0, false)

	if !strings.Contains(m3u8, "#EXTM3U") {
		t.Error("empty playlist should still have EXTM3U header")
	}
	if !strings.Contains(m3u8, "#EXT-X-PRELOAD-HINT") {
		t.Error("empty playlist should still have PRELOAD-HINT")
	}
}
