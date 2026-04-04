package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/im-pingo/liveforge/tools/testkit/push"
	"github.com/im-pingo/liveforge/tools/testkit/report"
	"github.com/im-pingo/liveforge/tools/testkit/source"
)

func runPush(args []string) {
	fs := flag.NewFlagSet("push", flag.ExitOnError)

	protocol := fs.String("protocol", "rtmp", "Push protocol (rtmp, rtsp, srt, whip, gb28181)")
	target := fs.String("target", "", "Target URL (e.g. rtmp://host/live/test)")
	duration := fs.Duration("duration", 0, "Push duration (e.g. 5s, 1m); 0 = until source exhausted")
	token := fs.String("token", "", "Auth token")
	output := fs.String("output", "", "Output format: human, json (default: auto-detect from TTY)")
	timeout := fs.Duration("timeout", 30*time.Second, "Overall timeout (e.g. 30s, 1m)")

	var asserts stringSlice
	fs.Var(&asserts, "assert", "Assertion expression (repeatable, e.g. 'push.frames_sent>0')")

	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	if *target == "" {
		fmt.Fprintln(os.Stderr, "Error: --target is required")
		fs.Usage()
		os.Exit(2)
	}

	// Create source: play FLV once.
	src := source.NewFLVSourceLoop(1)

	// Create protocol-specific pusher.
	pusher, err := push.NewPusher(*protocol)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(2)
	}

	// Build push config.
	cfg := push.PushConfig{
		Protocol: *protocol,
		Target:   *target,
		Duration: *duration,
		Token:    *token,
	}

	// Run push with timeout.
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	start := time.Now()
	pushReport, err := pusher.Push(ctx, src, cfg)
	elapsed := time.Since(start)

	// Build top-level report.
	topReport := &report.TopLevelReport{
		Command:    "push",
		Timestamp:  start,
		DurationMs: elapsed.Milliseconds(),
		Pass:       true,
		Push:       pushReport,
	}

	if err != nil {
		topReport.Pass = false
		topReport.Errors = append(topReport.Errors, report.ErrorDetail{
			Code:    classifyError(err),
			Message: err.Error(),
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

// classifyError maps common push errors to error codes.
func classifyError(err error) string {
	if err == context.DeadlineExceeded {
		return "TIMEOUT"
	}
	return "CONNECT_FAILED"
}

// printReport writes the report to stdout in the requested format.
func printReport(topReport *report.TopLevelReport, assertions []report.AssertionResult, format string) {
	switch format {
	case "json":
		data, err := report.FormatJSON(topReport)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error formatting JSON: %v\n", err)
			os.Exit(2)
		}
		fmt.Println(string(data))
	default:
		if len(assertions) > 0 {
			fmt.Print(report.FormatHumanWithAssertions(topReport, assertions))
		} else {
			fmt.Print(report.FormatHuman(topReport))
		}
	}
}
