package play

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/muxer/fmp4"
	"github.com/im-pingo/liveforge/pkg/muxer/ts"
)

// llhlsPlayer implements Player for Low-Latency HLS streaming. It fetches
// m3u8 playlists with blocking reload support, downloads partial segments,
// demuxes them (fmp4 or TS), and delivers AVFrames via the FrameCallback.
type llhlsPlayer struct{}

// llhlsPart represents a partial segment entry from an LL-HLS playlist.
type llhlsPart struct {
	Duration    float64
	URI         string
	Independent bool
	MSN         int // media sequence number of the parent segment
	PartIdx     int // index of this part within the parent segment
}

// llhlsPlaylist holds parsed data from an LL-HLS m3u8 playlist.
type llhlsPlaylist struct {
	mediaSequence int
	initURI       string // from EXT-X-MAP:URI, empty for TS container
	parts         []llhlsPart
	segments      []hlsSegment // full segments (reuses hlsSegment from hls.go)
	preloadHint   string       // from EXT-X-PRELOAD-HINT URI
}

// Play fetches the LL-HLS playlist, downloads partial/full segments, demuxes
// them, and delivers frames to onFrame. It uses blocking playlist reload
// (via _HLS_msn and _HLS_part query params) for low-latency segment discovery.
func (p *llhlsPlayer) Play(ctx context.Context, cfg PlayConfig, onFrame FrameCallback) error {
	baseURL, err := resolveBaseURL(cfg.URL)
	if err != nil {
		return fmt.Errorf("llhls: resolve base URL: %w", err)
	}

	m3u8URL, err := buildHLSURL(cfg.URL, cfg.Token)
	if err != nil {
		return fmt.Errorf("llhls: build m3u8 URL: %w", err)
	}

	playCtx := ctx
	if cfg.Duration > 0 {
		var cancel context.CancelFunc
		playCtx, cancel = context.WithTimeout(ctx, cfg.Duration)
		defer cancel()
	}

	// First fetch: non-blocking to discover container format and initial state.
	var playlist *llhlsPlaylist
	pollInterval := 500 * time.Millisecond
	for {
		select {
		case <-playCtx.Done():
			return nil
		default:
		}

		body, err := httpGet(playCtx, m3u8URL)
		if err != nil {
			if playCtx.Err() != nil {
				return nil
			}
			select {
			case <-playCtx.Done():
				return nil
			case <-time.After(pollInterval):
				continue
			}
		}

		playlist = parseLLHLSPlaylist(body)
		if len(playlist.parts) > 0 || len(playlist.segments) > 0 {
			break
		}

		// Playlist has no content yet; retry.
		select {
		case <-playCtx.Done():
			return nil
		case <-time.After(pollInterval):
		}
	}

	// Detect container format: fmp4 if EXT-X-MAP is present, otherwise TS.
	isFMP4 := playlist.initURI != ""

	// For fmp4: fetch init segment and create demuxer.
	var fmp4Demuxer *fmp4.Demuxer
	if isFMP4 {
		initURL := resolveSegmentURL(baseURL, playlist.initURI, cfg.Token)
		initData, err := httpGetBytes(playCtx, initURL)
		if err != nil {
			if playCtx.Err() != nil {
				return nil
			}
			return fmt.Errorf("llhls: fetch init segment: %w", err)
		}
		fmp4Demuxer, err = fmp4.NewDemuxer(initData)
		if err != nil {
			return fmt.Errorf("llhls: create fmp4 demuxer: %w", err)
		}
	}

	// Track last consumed MSN + part index for blocking reload.
	lastMSN := -1
	lastPartIdx := -1

	// Process the initial playlist, then loop with blocking reload.
	for {
		select {
		case <-playCtx.Done():
			return nil
		default:
		}

		// Process parts from current playlist.
		for _, part := range playlist.parts {
			if part.MSN < lastMSN || (part.MSN == lastMSN && part.PartIdx <= lastPartIdx) {
				continue
			}

			select {
			case <-playCtx.Done():
				return nil
			default:
			}

			partURL := resolveSegmentURL(baseURL, part.URI, cfg.Token)
			partData, err := httpGetBytes(playCtx, partURL)
			if err != nil {
				if playCtx.Err() != nil {
					return nil
				}
				// Skip unavailable parts (404 from eviction).
				lastMSN = part.MSN
				lastPartIdx = part.PartIdx
				continue
			}

			if isFMP4 {
				frames, err := fmp4Demuxer.Parse(partData)
				if err != nil {
					// Skip malformed segments.
					lastMSN = part.MSN
					lastPartIdx = part.PartIdx
					continue
				}
				for _, frame := range frames {
					onFrame(frame)
				}
			} else {
				tsDemuxer := ts.NewDemuxer(func(frame *avframe.AVFrame) {
					onFrame(frame)
				})
				tsDemuxer.Feed(partData)
				tsDemuxer.Flush()
			}

			lastMSN = part.MSN
			lastPartIdx = part.PartIdx
		}

		// If no parts were found, fall back to processing full segments.
		if len(playlist.parts) == 0 {
			for _, seg := range playlist.segments {
				seqKey := seg.SeqNum
				if seqKey <= lastMSN {
					continue
				}

				select {
				case <-playCtx.Done():
					return nil
				default:
				}

				segURL := resolveSegmentURL(baseURL, seg.URI, cfg.Token)
				segData, err := httpGetBytes(playCtx, segURL)
				if err != nil {
					if playCtx.Err() != nil {
						return nil
					}
					lastMSN = seqKey
					continue
				}

				if isFMP4 {
					frames, err := fmp4Demuxer.Parse(segData)
					if err != nil {
						lastMSN = seqKey
						continue
					}
					for _, frame := range frames {
						onFrame(frame)
					}
				} else {
					tsDemuxer := ts.NewDemuxer(func(frame *avframe.AVFrame) {
						onFrame(frame)
					})
					tsDemuxer.Feed(segData)
					tsDemuxer.Flush()
				}

				lastMSN = seqKey
				lastPartIdx = -1
			}
		}

		// Calculate next MSN + part for blocking reload.
		nextMSN, nextPart := calcNextBlockingParams(playlist, lastMSN, lastPartIdx)

		// Build blocking reload URL.
		reloadURL, err := buildBlockingReloadURL(m3u8URL, nextMSN, nextPart)
		if err != nil {
			return fmt.Errorf("llhls: build blocking reload URL: %w", err)
		}

		body, err := httpGet(playCtx, reloadURL)
		if err != nil {
			if playCtx.Err() != nil {
				return nil
			}
			// On error, fall back to non-blocking fetch after a short wait.
			select {
			case <-playCtx.Done():
				return nil
			case <-time.After(pollInterval):
			}
			body, err = httpGet(playCtx, m3u8URL)
			if err != nil {
				if playCtx.Err() != nil {
					return nil
				}
				continue
			}
		}

		playlist = parseLLHLSPlaylist(body)
	}
}

// parseLLHLSPlaylist parses an LL-HLS m3u8 playlist body, extracting
// EXT-X-MAP, EXT-X-MEDIA-SEQUENCE, EXT-X-PART, EXTINF, and EXT-X-PRELOAD-HINT.
func parseLLHLSPlaylist(body string) *llhlsPlaylist {
	pl := &llhlsPlaylist{}
	lines := strings.Split(strings.TrimSpace(body), "\n")

	currentMSN := 0
	partIdxInSegment := 0
	var pendingDuration float64
	hasPendingDuration := false
	segIdx := 0

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// EXT-X-MEDIA-SEQUENCE
		if strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:") {
			val := strings.TrimPrefix(line, "#EXT-X-MEDIA-SEQUENCE:")
			if n, err := strconv.Atoi(strings.TrimSpace(val)); err == nil {
				pl.mediaSequence = n
				currentMSN = n
			}
			continue
		}

		// EXT-X-MAP
		if strings.HasPrefix(line, "#EXT-X-MAP:") {
			pl.initURI = parseAttributeValue(line, "URI")
			continue
		}

		// EXT-X-PART
		if strings.HasPrefix(line, "#EXT-X-PART:") {
			part := llhlsPart{
				MSN:     currentMSN + segIdx,
				PartIdx: partIdxInSegment,
			}

			durStr := parseAttributeValue(line, "DURATION")
			if d, err := strconv.ParseFloat(durStr, 64); err == nil {
				part.Duration = d
			}

			part.URI = parseAttributeValue(line, "URI")
			part.Independent = strings.Contains(line, "INDEPENDENT=YES")

			pl.parts = append(pl.parts, part)
			partIdxInSegment++
			continue
		}

		// EXT-X-PRELOAD-HINT
		if strings.HasPrefix(line, "#EXT-X-PRELOAD-HINT:") {
			pl.preloadHint = parseAttributeValue(line, "URI")
			continue
		}

		// EXTINF
		if strings.HasPrefix(line, "#EXTINF:") {
			durStr := strings.TrimPrefix(line, "#EXTINF:")
			if commaIdx := strings.Index(durStr, ","); commaIdx >= 0 {
				durStr = durStr[:commaIdx]
			}
			if d, err := strconv.ParseFloat(strings.TrimSpace(durStr), 64); err == nil {
				pendingDuration = d
				hasPendingDuration = true
			}
			continue
		}

		// Skip other tags.
		if strings.HasPrefix(line, "#") {
			continue
		}

		// Segment URI line.
		if hasPendingDuration {
			pl.segments = append(pl.segments, hlsSegment{
				SeqNum:   pl.mediaSequence + segIdx,
				Duration: pendingDuration,
				URI:      line,
			})
			segIdx++
			partIdxInSegment = 0
			hasPendingDuration = false
		}
	}

	return pl
}

// parseAttributeValue extracts the value for a named attribute from an HLS tag.
// Handles both quoted (URI="value") and unquoted (DURATION=1.23) attribute values.
func parseAttributeValue(line, attr string) string {
	key := attr + "="
	idx := strings.Index(line, key)
	if idx < 0 {
		return ""
	}
	rest := line[idx+len(key):]

	if len(rest) > 0 && rest[0] == '"' {
		// Quoted value.
		end := strings.Index(rest[1:], "\"")
		if end < 0 {
			return rest[1:]
		}
		return rest[1 : end+1]
	}

	// Unquoted value: ends at comma or end of string.
	end := strings.IndexByte(rest, ',')
	if end < 0 {
		return rest
	}
	return rest[:end]
}

// calcNextBlockingParams determines the next MSN and part index to request
// in a blocking playlist reload, based on the last consumed position.
func calcNextBlockingParams(pl *llhlsPlaylist, lastMSN, lastPartIdx int) (nextMSN, nextPart int) {
	if len(pl.parts) == 0 {
		// No parts; request next full segment.
		return lastMSN + 1, 0
	}

	// Find the maximum MSN and part index in the current playlist.
	maxMSN := 0
	maxPartForMaxMSN := 0
	for _, part := range pl.parts {
		if part.MSN > maxMSN || (part.MSN == maxMSN && part.PartIdx > maxPartForMaxMSN) {
			maxMSN = part.MSN
			maxPartForMaxMSN = part.PartIdx
		}
	}

	// Request the next part after what we consumed.
	if lastMSN < maxMSN || (lastMSN == maxMSN && lastPartIdx < maxPartForMaxMSN) {
		// There are still unconsumed parts; request the next one we need.
		nextMSN = lastMSN
		nextPart = lastPartIdx + 1
	} else {
		// We consumed everything; request the next part after the last known.
		nextMSN = maxMSN
		nextPart = maxPartForMaxMSN + 1
	}

	return nextMSN, nextPart
}

// buildBlockingReloadURL appends _HLS_msn and _HLS_part query parameters
// to the m3u8 URL for blocking playlist reload.
func buildBlockingReloadURL(m3u8URL string, msn, part int) (string, error) {
	u, err := url.Parse(m3u8URL)
	if err != nil {
		return "", fmt.Errorf("invalid URL %q: %w", m3u8URL, err)
	}

	q := u.Query()
	q.Set("_HLS_msn", strconv.Itoa(msn))
	q.Set("_HLS_part", strconv.Itoa(part))
	u.RawQuery = q.Encode()

	return u.String(), nil
}
