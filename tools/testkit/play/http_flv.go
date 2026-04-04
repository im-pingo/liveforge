package play

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/im-pingo/liveforge/pkg/muxer/flv"
)

// httpFLVPlayer implements Player for HTTP-FLV streaming. It issues an HTTP GET
// request and feeds the chunked response body into the FLV demuxer, delivering
// each demuxed tag as an AVFrame via the FrameCallback.
type httpFLVPlayer struct{}

// Play connects to the target HTTP-FLV endpoint, reads the continuous FLV byte
// stream, demuxes it, and delivers AVFrames to onFrame. Duration is enforced
// via a child context timeout; the demuxer's blocking ReadTag naturally
// cooperates with context cancellation since closing the response body unblocks
// the underlying read.
func (p *httpFLVPlayer) Play(ctx context.Context, cfg PlayConfig, onFrame FrameCallback) error {
	targetURL, err := buildHTTPFLVURL(cfg.URL, cfg.Token)
	if err != nil {
		return fmt.Errorf("httpflv: build URL: %w", err)
	}

	// Create a child context with the configured duration so that the HTTP
	// request is cancelled automatically when the play duration expires.
	playCtx := ctx
	if cfg.Duration > 0 {
		var cancel context.CancelFunc
		playCtx, cancel = context.WithTimeout(ctx, cfg.Duration)
		defer cancel()
	}

	req, err := http.NewRequestWithContext(playCtx, http.MethodGet, targetURL, nil)
	if err != nil {
		return fmt.Errorf("httpflv: build request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// Context cancellation (duration expired) is a normal exit.
		if playCtx.Err() != nil {
			return nil
		}
		return fmt.Errorf("httpflv: GET %s: %w", targetURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("httpflv: unexpected status %d from %s", resp.StatusCode, targetURL)
	}

	demuxer := flv.NewDemuxer(resp.Body)

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
			return fmt.Errorf("httpflv: read tag: %w", err)
		}
		onFrame(frame)
	}
}

// buildHTTPFLVURL appends the token as a query parameter if provided.
func buildHTTPFLVURL(rawURL, token string) (string, error) {
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
