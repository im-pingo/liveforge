package dvr

import (
	"fmt"
	"math"
	"strings"
)

// GeneratePlaylist builds an HLS m3u8 playlist from the segment index.
func GeneratePlaylist(index *SegmentIndex, streamKey string, live bool) string {
	return GeneratePlaylistWithQuery(index, streamKey, live, "")
}

// GeneratePlaylistWithQuery appends an already URL-encoded query string to
// segment URIs so token-authenticated playlist requests remain authorized.
func GeneratePlaylistWithQuery(index *SegmentIndex, streamKey string, live bool, rawQuery string) string {
	segments := index.Segments()
	if len(segments) == 0 {
		return ""
	}

	// Extract the key portion (after app/) for relative segment URLs
	key := streamKey
	if i := strings.IndexByte(streamKey, '/'); i >= 0 {
		key = streamKey[i+1:]
	}

	maxDur := index.MaxDuration()
	targetDur := int(math.Ceil(maxDur))
	if targetDur < 1 {
		targetDur = 1
	}

	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:6\n")
	b.WriteString(fmt.Sprintf("#EXT-X-TARGETDURATION:%d\n", targetDur))
	b.WriteString(fmt.Sprintf("#EXT-X-MEDIA-SEQUENCE:%d\n", segments[0].SeqNum))

	for _, seg := range segments {
		b.WriteString(fmt.Sprintf("#EXT-X-PROGRAM-DATE-TIME:%s\n", seg.StartTime.UTC().Format("2006-01-02T15:04:05.000Z")))
		b.WriteString(fmt.Sprintf("#EXTINF:%.3f,\n", seg.Duration))
		segmentURI := fmt.Sprintf("%s/%s", key, seg.Filename)
		if rawQuery != "" {
			segmentURI += "?" + rawQuery
		}
		b.WriteString(segmentURI + "\n")
	}

	if !live {
		b.WriteString("#EXT-X-ENDLIST\n")
	}

	return b.String()
}
