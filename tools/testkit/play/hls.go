package play

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/muxer/ts"
)

// hlsPlayer implements Player for HLS streaming. It polls an m3u8 playlist,
// downloads new TS segments, demuxes them with the TS demuxer, and delivers
// AVFrames via the FrameCallback.
type hlsPlayer struct{}

// Play fetches the m3u8 playlist, downloads TS segments, demuxes them, and
// delivers frames to onFrame. It polls the playlist at approximately 1-second
// intervals and tracks which segments have already been fetched by media
// sequence number.
func (p *hlsPlayer) Play(ctx context.Context, cfg PlayConfig, onFrame FrameCallback) error {
	baseURL, err := resolveBaseURL(cfg.URL)
	if err != nil {
		return fmt.Errorf("hls: resolve base URL: %w", err)
	}

	// Build the m3u8 URL with optional token.
	m3u8URL, err := buildHLSURL(cfg.URL, cfg.Token)
	if err != nil {
		return fmt.Errorf("hls: build m3u8 URL: %w", err)
	}

	// Apply duration-based deadline.
	playCtx := ctx
	if cfg.Duration > 0 {
		var cancel context.CancelFunc
		playCtx, cancel = context.WithTimeout(ctx, cfg.Duration)
		defer cancel()
	}

	lastSeq := -1
	pollInterval := 1 * time.Second

	for {
		select {
		case <-playCtx.Done():
			return nil
		default:
		}

		// Fetch the m3u8 playlist. A 404 is expected when the HLS session
		// has not started yet; retry on the next poll.
		body, err := httpGet(playCtx, m3u8URL)
		if err != nil {
			if playCtx.Err() != nil {
				return nil
			}
			// Treat retriable HTTP errors (e.g. 404 while server generates
			// first segment) as transient: wait and retry.
			select {
			case <-playCtx.Done():
				return nil
			case <-time.After(pollInterval):
				continue
			}
		}

		_, segments := parseM3U8(body)

		// Filter to new segments only.
		for _, seg := range segments {
			if seg.SeqNum <= lastSeq {
				continue
			}

			select {
			case <-playCtx.Done():
				return nil
			default:
			}

			// Resolve segment URL against the base URL.
			segURL := resolveSegmentURL(baseURL, seg.URI, cfg.Token)

			// Download the TS segment. A 404 is normal when the segment
			// has been evicted from the server's sliding window (live
			// streams produce data fast and old segments are pruned).
			// Skip evicted segments instead of failing.
			segData, err := httpGetBytes(playCtx, segURL)
			if err != nil {
				if playCtx.Err() != nil {
					return nil
				}
				// Skip segments that are no longer available.
				lastSeq = seg.SeqNum
				continue
			}

			// Create a fresh demuxer per segment to avoid state leakage.
			demuxer := ts.NewDemuxer(func(frame *avframe.AVFrame) {
				onFrame(frame)
			})
			demuxer.Feed(segData)
			demuxer.Flush()

			lastSeq = seg.SeqNum
		}

		// Wait before polling again.
		select {
		case <-playCtx.Done():
			return nil
		case <-time.After(pollInterval):
		}
	}
}

// hlsSegment represents a single media segment parsed from an m3u8 playlist.
type hlsSegment struct {
	SeqNum   int
	Duration float64
	URI      string
}

// parseM3U8 extracts the media sequence number and segment list from an m3u8
// playlist body. It handles the standard HLS tags: EXT-X-MEDIA-SEQUENCE and
// EXTINF.
func parseM3U8(body string) (mediaSeq int, segments []hlsSegment) {
	lines := strings.Split(strings.TrimSpace(body), "\n")
	var pendingDuration float64
	hasPendingDuration := false
	idx := 0

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:") {
			val := strings.TrimPrefix(line, "#EXT-X-MEDIA-SEQUENCE:")
			if n, err := strconv.Atoi(strings.TrimSpace(val)); err == nil {
				mediaSeq = n
			}
			continue
		}

		if strings.HasPrefix(line, "#EXTINF:") {
			durStr := strings.TrimPrefix(line, "#EXTINF:")
			// Remove trailing comma and any title.
			if commaIdx := strings.Index(durStr, ","); commaIdx >= 0 {
				durStr = durStr[:commaIdx]
			}
			if d, err := strconv.ParseFloat(strings.TrimSpace(durStr), 64); err == nil {
				pendingDuration = d
				hasPendingDuration = true
			}
			continue
		}

		// Skip other comment/tag lines.
		if strings.HasPrefix(line, "#") {
			continue
		}

		// This is a segment URI line.
		if hasPendingDuration {
			segments = append(segments, hlsSegment{
				SeqNum:   mediaSeq + idx,
				Duration: pendingDuration,
				URI:      line,
			})
			idx++
			hasPendingDuration = false
		}
	}

	return mediaSeq, segments
}

// resolveBaseURL extracts the scheme + host portion from the m3u8 URL.
func resolveBaseURL(m3u8URL string) (string, error) {
	u, err := url.Parse(m3u8URL)
	if err != nil {
		return "", fmt.Errorf("invalid m3u8 URL %q: %w", m3u8URL, err)
	}
	return fmt.Sprintf("%s://%s", u.Scheme, u.Host), nil
}

// resolveSegmentURL builds the full URL for a segment. If the segment URI is
// an absolute path (starts with "/"), it is resolved against the base URL.
// Otherwise it is used as-is (assumed to be a full URL).
func resolveSegmentURL(baseURL, segURI, token string) string {
	var fullURL string
	if strings.HasPrefix(segURI, "/") {
		fullURL = baseURL + segURI
	} else {
		fullURL = segURI
	}

	if token != "" {
		if strings.Contains(fullURL, "?") {
			fullURL += "&token=" + url.QueryEscape(token)
		} else {
			fullURL += "?token=" + url.QueryEscape(token)
		}
	}

	return fullURL
}

// buildHLSURL appends the token as a query parameter to the m3u8 URL if
// provided.
func buildHLSURL(rawURL, token string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("invalid URL %q: %w", rawURL, err)
	}

	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("unsupported scheme %q, want http or https", u.Scheme)
	}

	if token != "" {
		q := u.Query()
		q.Set("token", token)
		u.RawQuery = q.Encode()
	}

	return u.String(), nil
}

// httpGet fetches the given URL and returns the response body as a string.
func httpGet(ctx context.Context, targetURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status %d from %s", resp.StatusCode, targetURL)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read body: %w", err)
	}

	return string(data), nil
}

// httpGetBytes fetches the given URL and returns the response body as bytes.
func httpGetBytes(ctx context.Context, targetURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d from %s", resp.StatusCode, targetURL)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	return data, nil
}
