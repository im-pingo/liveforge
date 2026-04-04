package play

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/coder/websocket"
	"github.com/im-pingo/liveforge/pkg/muxer/flv"
)

// wsFLVPlayer implements Player for WebSocket-FLV streaming. It dials a
// WebSocket endpoint, bridges the frame-based WS messages into a continuous
// io.Reader via an io.Pipe, and feeds the reader into the FLV demuxer.
type wsFLVPlayer struct{}

// Play connects to the target WS-FLV endpoint, reads WebSocket binary messages
// containing FLV data, pipes them into the FLV demuxer, and delivers each
// demuxed tag as an AVFrame via onFrame.
func (p *wsFLVPlayer) Play(ctx context.Context, cfg PlayConfig, onFrame FrameCallback) error {
	wsURL, err := buildWSFLVURL(cfg.URL, cfg.Token)
	if err != nil {
		return fmt.Errorf("wsflv: build URL: %w", err)
	}

	// Create a child context with the configured duration.
	playCtx := ctx
	if cfg.Duration > 0 {
		var cancel context.CancelFunc
		playCtx, cancel = context.WithTimeout(ctx, cfg.Duration)
		defer cancel()
	}

	conn, _, err := websocket.Dial(playCtx, wsURL, nil)
	if err != nil {
		if playCtx.Err() != nil {
			return nil
		}
		return fmt.Errorf("wsflv: dial %s: %w", wsURL, err)
	}
	defer conn.CloseNow()

	// Bridge WebSocket messages into a continuous reader for the FLV demuxer.
	pr, pw := io.Pipe()

	// Goroutine: read WS binary messages and write into the pipe.
	go func() {
		defer pw.Close()
		for {
			_, data, readErr := conn.Read(playCtx)
			if readErr != nil {
				pw.CloseWithError(readErr)
				return
			}
			if _, writeErr := pw.Write(data); writeErr != nil {
				return
			}
		}
	}()

	demuxer := flv.NewDemuxer(pr)

	for {
		frame, err := demuxer.ReadTag()
		if err != nil {
			// Context cancellation (duration expired) is a normal exit.
			if playCtx.Err() != nil {
				return nil
			}
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("wsflv: read tag: %w", err)
		}
		onFrame(frame)
	}
}

// buildWSFLVURL converts an http(s) URL to ws(s) and appends a token query
// parameter if provided.
func buildWSFLVURL(rawURL, token string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("invalid URL %q: %w", rawURL, err)
	}

	// Convert scheme: http -> ws, https -> wss.
	switch strings.ToLower(u.Scheme) {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	case "ws", "wss":
		// Already a WebSocket scheme; keep as-is.
	default:
		return "", fmt.Errorf("unsupported scheme %q, want http, https, ws, or wss", u.Scheme)
	}

	if token != "" {
		q := u.Query()
		q.Set("token", token)
		u.RawQuery = q.Encode()
	}

	return u.String(), nil
}
