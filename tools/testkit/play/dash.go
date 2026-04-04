package play

import (
	"context"
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/im-pingo/liveforge/pkg/muxer/fmp4"
)

// dashPlayer implements Player for DASH streaming. It polls the MPD manifest,
// downloads separate video and audio init segments, creates independent fmp4
// demuxers for each track, fetches new media segments, and delivers AVFrames
// via the FrameCallback.
type dashPlayer struct{}

// ---------------------------------------------------------------------------
// MPD XML structs (minimal, matches server output)
// ---------------------------------------------------------------------------

type mpdManifest struct {
	XMLName             xml.Name    `xml:"MPD"`
	Type                string      `xml:"type,attr"`
	MinimumUpdatePeriod string      `xml:"minimumUpdatePeriod,attr"`
	Periods             []mpdPeriod `xml:"Period"`
}

type mpdPeriod struct {
	AdaptationSets []mpdAdaptationSet `xml:"AdaptationSet"`
}

type mpdAdaptationSet struct {
	ContentType     string              `xml:"contentType,attr"`
	SegmentTemplate *mpdSegmentTemplate `xml:"SegmentTemplate"`
}

type mpdSegmentTemplate struct {
	StartNumber    int                  `xml:"startNumber,attr"`
	Initialization string               `xml:"initialization,attr"`
	Media          string               `xml:"media,attr"`
	Timeline       *mpdSegmentTimeline  `xml:"SegmentTimeline"`
}

type mpdSegmentTimeline struct {
	Entries []mpdTimelineEntry `xml:"S"`
}

type mpdTimelineEntry struct {
	T int64 `xml:"t,attr"` // start time (optional)
	D int64 `xml:"d,attr"` // duration
	R int   `xml:"r,attr"` // repeat count (0 = no repeat)
}

// ---------------------------------------------------------------------------
// dashTrackState tracks per-track (video or audio) init + demuxer + cursor
// ---------------------------------------------------------------------------

type dashTrackState struct {
	demuxer     *fmp4.Demuxer
	lastSegNum  int // last successfully fetched segment number
}

// ---------------------------------------------------------------------------
// Play
// ---------------------------------------------------------------------------

// Play fetches the DASH MPD manifest, downloads video and audio init segments,
// creates separate fmp4 demuxers, then polls the MPD to discover and fetch new
// media segments. Each segment is demuxed and frames are delivered via onFrame.
func (p *dashPlayer) Play(ctx context.Context, cfg PlayConfig, onFrame FrameCallback) error {
	baseURL, err := resolveBaseURL(cfg.URL)
	if err != nil {
		return fmt.Errorf("dash: resolve base URL: %w", err)
	}

	mpdURL := cfg.URL
	if cfg.Token != "" {
		sep := "?"
		if strings.Contains(mpdURL, "?") {
			sep = "&"
		}
		mpdURL += sep + "token=" + cfg.Token
	}

	playCtx := ctx
	if cfg.Duration > 0 {
		var cancel context.CancelFunc
		playCtx, cancel = context.WithTimeout(ctx, cfg.Duration)
		defer cancel()
	}

	// Default poll interval; will be refined from MPD minimumUpdatePeriod.
	pollInterval := 3 * time.Second

	// Per-track state. Initialised on first successful MPD fetch.
	var videoTrack *dashTrackState
	var audioTrack *dashTrackState
	initialised := false

	for {
		select {
		case <-playCtx.Done():
			return nil
		default:
		}

		// Fetch and parse MPD.
		body, err := httpGet(playCtx, mpdURL)
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

		mpd, err := parseMPD(body)
		if err != nil {
			// Malformed MPD; retry after interval.
			select {
			case <-playCtx.Done():
				return nil
			case <-time.After(pollInterval):
				continue
			}
		}

		// Refine poll interval from MPD minimumUpdatePeriod.
		if dur := parsePT(mpd.MinimumUpdatePeriod); dur > 0 {
			pollInterval = dur
		}

		// Walk AdaptationSets to find video and audio.
		var videoAS, audioAS *mpdAdaptationSet
		for i := range mpd.Periods {
			for j := range mpd.Periods[i].AdaptationSets {
				as := &mpd.Periods[i].AdaptationSets[j]
				switch as.ContentType {
				case "video":
					if videoAS == nil {
						videoAS = as
					}
				case "audio":
					if audioAS == nil {
						audioAS = as
					}
				}
			}
		}

		if videoAS == nil {
			// No video yet; retry.
			select {
			case <-playCtx.Done():
				return nil
			case <-time.After(pollInterval):
				continue
			}
		}

		// Initialise demuxers on first valid MPD.
		if !initialised {
			vt, err := initTrack(playCtx, baseURL, cfg.Token, videoAS)
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
			videoTrack = vt

			if audioAS != nil {
				at, err := initTrack(playCtx, baseURL, cfg.Token, audioAS)
				if err != nil {
					if playCtx.Err() != nil {
						return nil
					}
					// Audio init failure is non-fatal; proceed without audio.
				} else {
					audioTrack = at
				}
			}

			initialised = true
		}

		// Fetch new video segments.
		if videoAS.SegmentTemplate != nil && videoTrack != nil {
			fetchSegments(playCtx, baseURL, cfg.Token, videoAS.SegmentTemplate, videoTrack, onFrame)
		}

		// Fetch new audio segments.
		if audioAS != nil && audioAS.SegmentTemplate != nil && audioTrack != nil {
			fetchSegments(playCtx, baseURL, cfg.Token, audioAS.SegmentTemplate, audioTrack, onFrame)
		}

		// Wait before next MPD poll.
		select {
		case <-playCtx.Done():
			return nil
		case <-time.After(pollInterval):
		}
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// parseMPD unmarshals an MPD XML document.
func parseMPD(body string) (*mpdManifest, error) {
	var mpd mpdManifest
	if err := xml.Unmarshal([]byte(body), &mpd); err != nil {
		return nil, fmt.Errorf("parse MPD: %w", err)
	}
	return &mpd, nil
}

// parsePT parses a simple ISO 8601 duration like "PT6S" or "PT3S" into a
// time.Duration. Only supports the PTnS format that the server generates.
func parsePT(s string) time.Duration {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "PT") || !strings.HasSuffix(s, "S") {
		return 0
	}
	inner := s[2 : len(s)-1]
	sec, err := strconv.ParseFloat(inner, 64)
	if err != nil {
		return 0
	}
	return time.Duration(sec * float64(time.Second))
}

// timelineSegmentCount returns the total number of segments described by a
// SegmentTimeline.
func timelineSegmentCount(tl *mpdSegmentTimeline) int {
	if tl == nil {
		return 0
	}
	count := 0
	for _, e := range tl.Entries {
		count += 1 + e.R
	}
	return count
}

// initTrack fetches the init segment for an AdaptationSet and creates an
// fmp4.Demuxer for it.
func initTrack(ctx context.Context, baseURL, token string, as *mpdAdaptationSet) (*dashTrackState, error) {
	if as.SegmentTemplate == nil {
		return nil, fmt.Errorf("dash: no SegmentTemplate in AdaptationSet")
	}

	initURI := as.SegmentTemplate.Initialization
	initURL := resolveSegmentURL(baseURL, initURI, token)

	initData, err := httpGetBytes(ctx, initURL)
	if err != nil {
		return nil, fmt.Errorf("dash: fetch init segment %s: %w", initURL, err)
	}

	demuxer, err := fmp4.NewDemuxer(initData)
	if err != nil {
		return nil, fmt.Errorf("dash: create demuxer from init segment: %w", err)
	}

	return &dashTrackState{
		demuxer:    demuxer,
		lastSegNum: 0, // no segments fetched yet
	}, nil
}

// fetchSegments downloads any new media segments for a track, demuxes them,
// and delivers the resulting frames.
func fetchSegments(ctx context.Context, baseURL, token string, tmpl *mpdSegmentTemplate, track *dashTrackState, onFrame FrameCallback) {
	count := timelineSegmentCount(tmpl.Timeline)
	if count == 0 {
		return
	}

	startNum := tmpl.StartNumber
	endNum := startNum + count - 1

	for segNum := startNum; segNum <= endNum; segNum++ {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Skip segments we have already fetched.
		if segNum <= track.lastSegNum {
			continue
		}

		// Build segment URL by replacing $Number$ in the media template.
		segURI := strings.Replace(tmpl.Media, "$Number$", strconv.Itoa(segNum), 1)
		segURL := resolveSegmentURL(baseURL, segURI, token)

		segData, err := httpGetBytes(ctx, segURL)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// Skip segments that are no longer available (404 from sliding window).
			track.lastSegNum = segNum
			continue
		}

		frames, err := track.demuxer.Parse(segData)
		if err != nil {
			// Skip malformed segments.
			track.lastSegNum = segNum
			continue
		}

		for _, frame := range frames {
			onFrame(frame)
		}

		track.lastSegNum = segNum
	}
}
