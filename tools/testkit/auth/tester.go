package auth

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/im-pingo/liveforge/tools/testkit/push"
	"github.com/im-pingo/liveforge/tools/testkit/report"
	"github.com/im-pingo/liveforge/tools/testkit/source"
)

// AuthTestConfig describes the server addresses and credentials for an auth
// test run.
type AuthTestConfig struct {
	ServerAddrs map[string]string // protocol -> addr (e.g. "rtmp" -> "127.0.0.1:1935")
	Secret      string
	StreamKey   string
	Protocols   []string // which protocols to test (e.g. ["rtmp", "srt", "http"])
}

// protocolActions maps each protocol to the actions it can test.
var protocolActions = map[string][]string{
	"rtmp": {"publish"},
	"srt":  {"publish"},
	"http": {"subscribe"},
}

// RunAuthTests runs all auth test cases across the configured protocols and
// returns an AuthReport with per-case results.
//
// Publish probes are run first so they don't conflict with the background
// publisher needed by subscribe probes. Subscribe probes run second, after a
// background publish stream has been established.
func RunAuthTests(ctx context.Context, cfg AuthTestConfig) (*report.AuthReport, error) {
	rpt := &report.AuthReport{}

	// Phase 1: Run all publish probes (no background publisher needed).
	runProbes(ctx, cfg, "publish", rpt)

	// Phase 2: Run all subscribe probes (need a background publisher).
	if needsSubscribeProbes(cfg.Protocols) {
		cancel, err := startBackgroundPublisher(ctx, cfg)
		if err != nil {
			return rpt, fmt.Errorf("start background publisher for subscribe probes: %w", err)
		}
		defer cancel()

		// Give the publisher time to establish the stream so subscribe probes
		// find an active stream.
		select {
		case <-ctx.Done():
			return rpt, ctx.Err()
		case <-time.After(1 * time.Second):
		}

		runProbes(ctx, cfg, "subscribe", rpt)
	}

	return rpt, nil
}

// runProbes runs all test cases for the given action across configured protocols.
func runProbes(ctx context.Context, cfg AuthTestConfig, targetAction string, rpt *report.AuthReport) {
	for _, proto := range cfg.Protocols {
		addr, ok := cfg.ServerAddrs[proto]
		if !ok || addr == "" {
			continue
		}

		actions, ok := protocolActions[proto]
		if !ok {
			continue
		}

		for _, action := range actions {
			if action != targetAction {
				continue
			}

			probe, ok := lookupProbe(proto, action)
			if !ok {
				continue
			}

			cases := GenerateCases(cfg.Secret, cfg.StreamKey, action)
			for _, tc := range cases {
				result := runSingleCase(ctx, probe, addr, cfg.StreamKey, action, proto, tc)
				rpt.Cases = append(rpt.Cases, result)
				rpt.Total++
				if result.Pass {
					rpt.Passed++
				} else {
					rpt.Failed++
				}
			}
		}
	}
}

// runSingleCase executes a single auth probe and builds a CaseResult.
func runSingleCase(
	ctx context.Context,
	probe ProbeFunc,
	addr, streamKey, action, protocol string,
	tc TestCase,
) report.CaseResult {
	start := time.Now()

	allowed, err := probe(ctx, addr, streamKey, action, tc.Token)

	result := report.CaseResult{
		Protocol:    protocol,
		Action:      action,
		Credential:  tc.Name,
		ExpectAllow: tc.ExpectAllow,
		ActualAllow: allowed,
		Pass:        allowed == tc.ExpectAllow,
		LatencyMs:   time.Since(start).Milliseconds(),
	}

	if err != nil {
		result.Error = err.Error()
		// Infrastructure errors are failures regardless of allow/deny.
		result.Pass = false
	}

	return result
}

// needsSubscribeProbes returns true if any of the requested protocols have
// subscribe actions.
func needsSubscribeProbes(protocols []string) bool {
	for _, p := range protocols {
		actions := protocolActions[p]
		for _, a := range actions {
			if a == "subscribe" {
				return true
			}
		}
	}
	return false
}

// startBackgroundPublisher starts an RTMP push with a valid token in a
// goroutine. It returns a cancel function that stops the publisher.
// The publisher uses the first available publish protocol address from the
// config (prefers RTMP).
func startBackgroundPublisher(ctx context.Context, cfg AuthTestConfig) (context.CancelFunc, error) {
	// Find an RTMP address for the background publisher.
	rtmpAddr, ok := cfg.ServerAddrs["rtmp"]
	if !ok || rtmpAddr == "" {
		// Try SRT as fallback; but RTMP is simpler for background publishing.
		return nil, fmt.Errorf("no RTMP address available for background publisher")
	}

	validToken := GenerateJWT(cfg.Secret, cfg.StreamKey, "publish", time.Now().Add(10*time.Minute))

	pusher, err := push.NewPusher("rtmp")
	if err != nil {
		return nil, fmt.Errorf("create rtmp pusher: %w", err)
	}

	pubCtx, cancel := context.WithCancel(ctx)
	src := source.NewFLVSourceLoop(0) // infinite loop

	target := fmt.Sprintf("rtmp://%s/%s", rtmpAddr, cfg.StreamKey)
	pushCfg := push.PushConfig{
		Protocol: "rtmp",
		Target:   target,
		Token:    validToken,
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Ignore push errors — we only need the stream to exist long enough
		// for subscribe probes.
		pusher.Push(pubCtx, src, pushCfg) //nolint:errcheck
	}()

	return func() {
		cancel()
		wg.Wait()
	}, nil
}
