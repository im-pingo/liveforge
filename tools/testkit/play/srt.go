package play

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"time"

	gosrt "github.com/datarhei/gosrt"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/muxer/ts"
)

// srtPlayer implements Player for the SRT protocol. It dials an SRT server in
// subscriber mode, reads MPEG-TS data from the connection, demuxes it into
// AVFrames, and delivers them to the FrameCallback.
type srtPlayer struct{}

// Play connects to the target SRT server, subscribes to the stream via the
// streamid, and delivers received media frames to onFrame. It respects ctx
// cancellation and cfg.Duration limits.
func (p *srtPlayer) Play(ctx context.Context, cfg PlayConfig, onFrame FrameCallback) error {
	addr, streamID, err := parseSRTPlayTarget(cfg.URL, cfg.Token)
	if err != nil {
		return fmt.Errorf("parse SRT URL: %w", err)
	}

	srtCfg := gosrt.DefaultConfig()
	srtCfg.StreamId = streamID

	conn, err := gosrt.Dial("srt", addr, srtCfg)
	if err != nil {
		return fmt.Errorf("srt dial %s: %w", addr, err)
	}
	defer conn.Close()

	demuxer := ts.NewDemuxer(func(frame *avframe.AVFrame) {
		onFrame(frame)
	})

	var deadline time.Time
	if cfg.Duration > 0 {
		deadline = time.Now().Add(cfg.Duration)
	}

	// Read SRT data in a goroutine since gosrt's SetReadDeadline is a no-op
	// and Read blocks until data arrives or the connection is closed. Use a
	// separate goroutine to allow ctx/deadline cancellation.
	type readResult struct {
		n   int
		err error
	}

	buf := make([]byte, 65536) // 64KB for TS packets
	readCh := make(chan readResult, 1)

	// readOnce launches a single blocking read and sends the result.
	readOnce := func() {
		n, err := conn.Read(buf)
		readCh <- readResult{n: n, err: err}
	}

	// Set up deadline timer once if duration is configured.
	var deadlineCh <-chan time.Time
	if !deadline.IsZero() {
		timer := time.NewTimer(time.Until(deadline))
		defer timer.Stop()
		deadlineCh = timer.C
	}

	go readOnce()

	for {
		select {
		case <-ctx.Done():
			conn.Close()
			demuxer.Flush()
			return ctx.Err()

		case <-deadlineCh:
			conn.Close()
			demuxer.Flush()
			return nil

		case res := <-readCh:
			if res.err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if res.err == io.EOF {
					demuxer.Flush()
					return nil
				}
				demuxer.Flush()
				return fmt.Errorf("srt read: %w", res.err)
			}

			demuxer.Feed(buf[:res.n])

			// Start the next read.
			go readOnce()
		}
	}
}

// parseSRTPlayTarget parses an SRT URL into a dial address and stream ID.
// Format: srt://host:port?streamid=subscribe:live/test[&token=xxx]
// If token is provided and the streamid does not already contain a token query
// parameter, it is appended as ?token=<url-encoded-token>.
func parseSRTPlayTarget(rawURL, token string) (addr, streamID string, err error) {
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

	// Append token from URL query if present.
	if urlToken := u.Query().Get("token"); urlToken != "" {
		streamID += "?token=" + urlToken
	} else if token != "" {
		// Append token from PlayConfig if provided and not already in URL.
		streamID += "?token=" + url.QueryEscape(token)
	}

	return addr, streamID, nil
}
