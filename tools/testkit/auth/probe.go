package auth

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/im-pingo/liveforge/tools/testkit/push"
	"github.com/im-pingo/liveforge/tools/testkit/source"
)

// probeTimeout is how long to wait for a push/play attempt to determine
// whether auth was accepted or rejected. If the operation has not failed
// within this window, we treat it as accepted.
const probeTimeout = 3 * time.Second

// ProbeFunc tests whether a connection with the given token is accepted.
// Returns (allowed, err). err is non-nil only for infrastructure failures,
// not for auth rejections.
type ProbeFunc func(ctx context.Context, addr, streamKey, action, token string) (bool, error)

// probeRegistry maps "protocol:action" to the appropriate probe function.
var probeRegistry = map[string]ProbeFunc{
	"rtmp:publish":  probeRTMPPublish,
	"srt:publish":   probeSRTPublish,
	"http:subscribe": probeHTTPFLVSubscribe,
}

// lookupProbe returns the probe function for the given protocol and action.
func lookupProbe(protocol, action string) (ProbeFunc, bool) {
	key := protocol + ":" + action
	fn, ok := probeRegistry[key]
	return fn, ok
}

// probeRTMPPublish attempts an RTMP publish with the given token.
// Auth rejection causes the RTMP handshake/publish sequence to return an error
// immediately. If the push runs for probeTimeout without error, auth is accepted.
func probeRTMPPublish(ctx context.Context, addr, streamKey, action, token string) (bool, error) {
	pusher, err := push.NewPusher("rtmp")
	if err != nil {
		return false, fmt.Errorf("create rtmp pusher: %w", err)
	}

	src := source.NewFLVSourceLoop(0)
	target := fmt.Sprintf("rtmp://%s/%s", addr, streamKey)

	cfg := push.PushConfig{
		Protocol: "rtmp",
		Target:   target,
		Duration: probeTimeout,
		Token:    token,
	}

	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	_, pushErr := pusher.Push(probeCtx, src, cfg)

	// If the push ran until the context deadline, auth was accepted.
	if pushErr == nil || probeCtx.Err() != nil {
		return true, nil
	}

	// Any other error means auth was rejected (or connection was refused).
	return false, nil
}

// probeSRTPublish attempts an SRT publish with the given token embedded in the
// streamid. Auth rejection causes the SRT dial or first write to fail.
func probeSRTPublish(ctx context.Context, addr, streamKey, action, token string) (bool, error) {
	pusher, err := push.NewPusher("srt")
	if err != nil {
		return false, fmt.Errorf("create srt pusher: %w", err)
	}

	src := source.NewFLVSourceLoop(0)

	// SRT target URL: srt://host:port?streamid=publish:streamKey[&token=xxx]
	targetURL := fmt.Sprintf("srt://%s?streamid=publish:%s", addr, streamKey)
	if token != "" {
		targetURL += "&token=" + url.QueryEscape(token)
	}

	cfg := push.PushConfig{
		Protocol: "srt",
		Target:   targetURL,
		Duration: probeTimeout,
	}

	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	_, pushErr := pusher.Push(probeCtx, src, cfg)

	if pushErr == nil || probeCtx.Err() != nil {
		return true, nil
	}

	return false, nil
}

// probeHTTPFLVSubscribe attempts an HTTP-FLV GET with the given token.
// The server returns 200 if allowed, or a non-200 status (typically 403) if rejected.
// Note: for subscribe probes, a stream must already be publishing.
func probeHTTPFLVSubscribe(ctx context.Context, addr, streamKey, action, token string) (bool, error) {
	targetURL := fmt.Sprintf("http://%s/%s.flv", addr, streamKey)
	if token != "" {
		targetURL += "?token=" + url.QueryEscape(token)
	}

	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, targetURL, nil)
	if err != nil {
		return false, fmt.Errorf("build http request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// Timeout or connection error — if the context expired while reading
		// an OK response body, that counts as allowed (the server accepted us
		// and started streaming).
		if probeCtx.Err() != nil {
			return true, nil
		}
		return false, fmt.Errorf("http GET %s: %w", targetURL, err)
	}
	defer func() {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()

	// 200 means auth accepted and the server is streaming to us.
	if resp.StatusCode == http.StatusOK {
		return true, nil
	}

	// Non-200 (401, 403, etc.) means auth was rejected.
	return false, nil
}
