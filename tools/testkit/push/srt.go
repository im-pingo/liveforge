package push

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"time"

	gosrt "github.com/datarhei/gosrt"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/muxer/ts"
	"github.com/im-pingo/liveforge/tools/testkit/report"
	"github.com/im-pingo/liveforge/tools/testkit/source"
)

// srtPusher implements Pusher for the SRT protocol. It dials an SRT server,
// muxes frames into MPEG-TS, and writes the TS packets over the SRT connection.
type srtPusher struct{}

// Push connects to the target SRT server, publishes frames from src via TS
// muxing, and returns a PushReport with statistics.
func (p *srtPusher) Push(ctx context.Context, src source.Source, cfg PushConfig) (*report.PushReport, error) {
	addr, streamID, err := parseSRTTarget(cfg.Target)
	if err != nil {
		return nil, fmt.Errorf("parse SRT URL: %w", err)
	}

	srtCfg := gosrt.DefaultConfig()
	srtCfg.StreamId = streamID

	conn, err := gosrt.Dial("srt", addr, srtCfg)
	if err != nil {
		return nil, fmt.Errorf("srt dial %s: %w", addr, err)
	}
	defer conn.Close()

	// Collect sequence headers from the source before creating the TS muxer.
	// The muxer needs codec config bytes (SPS/PPS for H.264, AudioSpecificConfig
	// for AAC) at construction time.
	mi := src.MediaInfo()
	var videoSeqData, audioSeqData []byte
	var muxer *ts.Muxer

	start := time.Now()
	var framesSent int64
	var bytesSent int64

	var deadline time.Time
	if cfg.Duration > 0 {
		deadline = start.Add(cfg.Duration)
	}

	for {
		select {
		case <-ctx.Done():
			return buildPushReport(cfg, start, framesSent, bytesSent), ctx.Err()
		default:
		}

		if !deadline.IsZero() && time.Now().After(deadline) {
			break
		}

		frame, err := src.NextFrame()
		if err == io.EOF {
			break
		}
		if err != nil {
			return buildPushReport(cfg, start, framesSent, bytesSent),
				fmt.Errorf("read source frame: %w", err)
		}

		// Capture sequence headers for the TS muxer.
		if frame.FrameType == avframe.FrameTypeSequenceHeader {
			if frame.MediaType.IsVideo() {
				videoSeqData = frame.Payload
			} else if frame.MediaType.IsAudio() {
				audioSeqData = frame.Payload
			}
			continue
		}

		// Lazily create the muxer once we encounter the first keyframe, by which
		// point the sequence headers should have been emitted by the source.
		if muxer == nil {
			if !frame.FrameType.IsKeyframe() {
				continue
			}
			muxer = ts.NewMuxer(mi.VideoCodec, mi.AudioCodec, videoSeqData, audioSeqData)
		}

		data := muxer.WriteFrame(frame)
		if len(data) == 0 {
			continue
		}

		n, err := conn.Write(data)
		if err != nil {
			return buildPushReport(cfg, start, framesSent, bytesSent),
				fmt.Errorf("srt write: %w", err)
		}

		framesSent++
		bytesSent += int64(n)
	}

	return buildPushReport(cfg, start, framesSent, bytesSent), nil
}

// parseSRTTarget parses an SRT URL into a dial address and stream ID.
// Format: srt://host:port?streamid=publish:live/test[&token=xxx]
// The streamid query parameter is used directly as the SRT stream identifier.
// If a token query parameter is present and the streamid does not already
// contain it, the token is appended as a query suffix.
func parseSRTTarget(rawURL string) (addr, streamID string, err error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", "", fmt.Errorf("invalid SRT URL: %w", err)
	}
	if u.Scheme != "srt" {
		return "", "", fmt.Errorf("unsupported scheme %q, want srt", u.Scheme)
	}
	if u.Host == "" {
		return "", "", fmt.Errorf("missing host in SRT URL")
	}

	addr = u.Host
	if u.Port() == "" {
		addr += ":6000"
	}

	streamID = u.Query().Get("streamid")
	if streamID == "" {
		return "", "", fmt.Errorf("missing streamid in SRT URL query parameters")
	}

	// Append token to streamid if provided separately and not already embedded.
	if token := u.Query().Get("token"); token != "" {
		streamID += "?token=" + token
	}

	return addr, streamID, nil
}
