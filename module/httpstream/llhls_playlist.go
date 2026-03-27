package httpstream

import (
	"fmt"
	"math"
	"strings"
)

// LLHLSPart represents a partial segment.
type LLHLSPart struct {
	Index       int     // part index within parent segment
	Duration    float64 // actual duration in seconds
	Independent bool    // starts with keyframe (IDR)
	Data        []byte  // muxed bytes
}

// LLHLSSegment represents a completed full segment.
type LLHLSSegment struct {
	MSN      int          // media sequence number
	Duration float64      // total duration in seconds
	Parts    []*LLHLSPart // partial segments
}

// LLHLSPlaylist generates m3u8 playlists with LL-HLS tags.
type LLHLSPlaylist struct {
	partTarget float64 // EXT-X-PART-INF PART-TARGET
	basePath   string  // URL prefix for segments, e.g. "/live/stream1"
	container  string  // "fmp4" or "ts"
}

// NewLLHLSPlaylist creates a new playlist generator.
func NewLLHLSPlaylist(partTarget float64, basePath, container string) *LLHLSPlaylist {
	return &LLHLSPlaylist{
		partTarget: partTarget,
		basePath:   basePath,
		container:  container,
	}
}

// Generate produces the m3u8 playlist text.
//
// Parameters:
//   - segments: completed segments in the sliding window
//   - currentParts: partial segments of the in-progress segment
//   - currentMSN: MSN of the in-progress segment
//   - skip: if true, emit EXT-X-SKIP for delta updates (client sent _HLS_skip=YES)
func (p *LLHLSPlaylist) Generate(segments []*LLHLSSegment, currentParts []*LLHLSPart, currentMSN int, skip bool) string {
	var sb strings.Builder

	sb.WriteString("#EXTM3U\n")
	sb.WriteString("#EXT-X-VERSION:9\n")

	// Compute EXT-X-TARGETDURATION from actual max segment duration
	maxDur := 6.0
	for _, seg := range segments {
		if seg.Duration > maxDur {
			maxDur = seg.Duration
		}
	}
	targetDur := int(math.Ceil(maxDur))
	fmt.Fprintf(&sb, "#EXT-X-TARGETDURATION:%d\n", targetDur)

	// EXT-X-PART-INF
	fmt.Fprintf(&sb, "#EXT-X-PART-INF:PART-TARGET=%.3f\n", p.partTarget)

	// EXT-X-SERVER-CONTROL
	partHoldBack := 3 * p.partTarget
	canSkipUntil := 6 * float64(targetDur)
	fmt.Fprintf(&sb, "#EXT-X-SERVER-CONTROL:CAN-BLOCK-RELOAD=YES,PART-HOLD-BACK=%.3f,CAN-SKIP-UNTIL=%.1f\n",
		partHoldBack, canSkipUntil)

	// EXT-X-MAP (fMP4 only)
	if p.container == "fmp4" {
		fmt.Fprintf(&sb, "#EXT-X-MAP:URI=\"%s/init.mp4\"\n", p.basePath)
	}

	// EXT-X-MEDIA-SEQUENCE
	seqBase := 0
	if len(segments) > 0 {
		seqBase = segments[0].MSN
	}
	fmt.Fprintf(&sb, "#EXT-X-MEDIA-SEQUENCE:%d\n", seqBase)

	// Delta update: skip old segments
	skipCount := 0
	startIdx := 0
	if skip && len(segments) > 1 {
		skipCount = max(len(segments)-2, 0)
		startIdx = skipCount
		if skipCount > 0 {
			fmt.Fprintf(&sb, "#EXT-X-SKIP:SKIPPED-SEGMENTS=%d\n", skipCount)
		}
	}

	sb.WriteString("\n")

	// Segment extension
	ext := "m4s"
	if p.container == "ts" {
		ext = "ts"
	}

	// Completed segments
	for i := startIdx; i < len(segments); i++ {
		seg := segments[i]
		for _, part := range seg.Parts {
			p.writePart(&sb, seg.MSN, part, ext)
		}
		fmt.Fprintf(&sb, "#EXTINF:%.3f,\n", seg.Duration)
		fmt.Fprintf(&sb, "%s/%d.%s\n", p.basePath, seg.MSN, ext)
	}

	// Current in-progress segment's partial segments
	for _, part := range currentParts {
		p.writePart(&sb, currentMSN, part, ext)
	}

	// EXT-X-PRELOAD-HINT for next expected partial
	nextPartIdx := len(currentParts)
	fmt.Fprintf(&sb, "#EXT-X-PRELOAD-HINT:TYPE=PART,URI=\"%s/%d.%d.%s\"\n",
		p.basePath, currentMSN, nextPartIdx, ext)

	return sb.String()
}

func (p *LLHLSPlaylist) writePart(sb *strings.Builder, msn int, part *LLHLSPart, ext string) {
	sb.WriteString("#EXT-X-PART:")
	fmt.Fprintf(sb, "DURATION=%.5f", part.Duration)
	fmt.Fprintf(sb, ",URI=\"%s/%d.%d.%s\"", p.basePath, msn, part.Index, ext)
	if part.Independent {
		sb.WriteString(",INDEPENDENT=YES")
	}
	sb.WriteString("\n")
}
