package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/im-pingo/liveforge/tools/testkit/analyzer"
	"github.com/im-pingo/liveforge/tools/testkit/play"
	"github.com/im-pingo/liveforge/tools/testkit/report"
)

func runPlay(args []string) {
	fs := flag.NewFlagSet("play", flag.ExitOnError)

	protocol := fs.String("protocol", "rtmp", "Play protocol (rtmp, rtsp, srt, whep, httpflv, wsflv, hls, llhls, dash)")
	url := fs.String("url", "", "Stream URL (e.g. rtmp://host/live/test)")
	duration := fs.Duration("duration", 0, "Play duration (e.g. 5s, 1m); 0 = until server closes")
	token := fs.String("token", "", "Auth token")
	output := fs.String("output", "", "Output format: human, json (default: auto-detect from TTY)")
	timeout := fs.Duration("timeout", 30*time.Second, "Overall timeout (e.g. 30s, 1m)")

	var asserts stringSlice
	fs.Var(&asserts, "assert", "Assertion expression (repeatable, e.g. 'video.fps>=29')")

	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	if *url == "" {
		fmt.Fprintln(os.Stderr, "Error: --url is required")
		fs.Usage()
		os.Exit(2)
	}

	// Create protocol-specific player.
	player, err := play.NewPlayer(*protocol)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(2)
	}

	// Create analyzer to collect stream statistics.
	a := analyzer.New()

	// Build play config.
	cfg := play.PlayConfig{
		Protocol: *protocol,
		URL:      *url,
		Duration: *duration,
		Token:    *token,
	}

	// Run play with timeout.
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	start := time.Now()
	playErr := player.Play(ctx, cfg, a.Feed)
	elapsed := time.Since(start)

	// Build top-level report.
	playReport := a.Report()
	if playErr != nil {
		playReport.Error = playErr.Error()
	}

	topReport := &report.TopLevelReport{
		Command:    "play",
		Timestamp:  start,
		DurationMs: elapsed.Milliseconds(),
		Pass:       true,
		Play:       playReport,
	}

	if playErr != nil {
		topReport.Pass = false
		topReport.Errors = append(topReport.Errors, report.ErrorDetail{
			Code:    classifyPlayError(playErr),
			Message: playErr.Error(),
		})
	}

	// Evaluate assertions.
	var results []report.AssertionResult
	allPass := true
	if len(asserts) > 0 {
		results, allPass = report.EvalAssertions(topReport, asserts)
		if !allPass {
			topReport.Pass = false
		}
	}

	// Output report.
	printReport(topReport, results, outputFormat(*output))

	// Exit code: 0 = pass, 1 = assertion failure, 2 = error (handled above).
	if !topReport.Pass {
		os.Exit(1)
	}
}

// classifyPlayError maps common play errors to error codes.
func classifyPlayError(err error) string {
	if err == context.DeadlineExceeded {
		return "TIMEOUT"
	}
	return "CONNECT_FAILED"
}
