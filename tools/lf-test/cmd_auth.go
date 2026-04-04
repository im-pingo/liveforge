package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/im-pingo/liveforge/tools/testkit/auth"
	"github.com/im-pingo/liveforge/tools/testkit/report"
)

func runAuth(args []string) {
	fs := flag.NewFlagSet("auth", flag.ExitOnError)

	secret := fs.String("secret", "", "JWT secret for generating test tokens (required)")
	stream := fs.String("stream", "live/test", "Stream key for auth test cases")
	serverRTMP := fs.String("server-rtmp", "", "RTMP server address (e.g. 127.0.0.1:1935)")
	serverSRT := fs.String("server-srt", "", "SRT server address (e.g. 127.0.0.1:6000)")
	serverHTTP := fs.String("server-http", "", "HTTP server address for HTTP-FLV subscribe (e.g. 127.0.0.1:8080)")
	output := fs.String("output", "", "Output format: human, json (default: auto-detect from TTY)")
	timeout := fs.Duration("timeout", 60*time.Second, "Overall timeout (e.g. 60s, 2m)")

	var asserts stringSlice
	fs.Var(&asserts, "assert", "Assertion expression (repeatable, e.g. 'auth.passed>=5')")

	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	if *secret == "" {
		fmt.Fprintln(os.Stderr, "Error: --secret is required")
		fs.Usage()
		os.Exit(2)
	}

	// Build server address map and protocol list from provided flags.
	serverAddrs := make(map[string]string)
	var protocols []string

	if *serverRTMP != "" {
		serverAddrs["rtmp"] = *serverRTMP
		protocols = append(protocols, "rtmp")
	}
	if *serverSRT != "" {
		serverAddrs["srt"] = *serverSRT
		protocols = append(protocols, "srt")
	}
	if *serverHTTP != "" {
		serverAddrs["http"] = *serverHTTP
		protocols = append(protocols, "http")
	}

	if len(protocols) == 0 {
		fmt.Fprintln(os.Stderr, "Error: at least one --server-* flag is required")
		fs.Usage()
		os.Exit(2)
	}

	cfg := auth.AuthTestConfig{
		ServerAddrs: serverAddrs,
		Secret:      *secret,
		StreamKey:   *stream,
		Protocols:   protocols,
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	start := time.Now()
	authReport, err := auth.RunAuthTests(ctx, cfg)
	elapsed := time.Since(start)

	topReport := &report.TopLevelReport{
		Command:    "auth",
		Timestamp:  start,
		DurationMs: elapsed.Milliseconds(),
		Pass:       true,
		Auth:       authReport,
	}

	if err != nil {
		topReport.Pass = false
		topReport.Errors = append(topReport.Errors, report.ErrorDetail{
			Code:    classifyAuthError(err),
			Message: err.Error(),
		})
	}

	// Mark as failed if any auth cases failed.
	if authReport != nil && authReport.Failed > 0 {
		topReport.Pass = false
	}

	// Evaluate assertions.
	var results []report.AssertionResult
	if len(asserts) > 0 {
		var allPass bool
		results, allPass = report.EvalAssertions(topReport, asserts)
		if !allPass {
			topReport.Pass = false
		}
	}

	printReport(topReport, results, outputFormat(*output))

	if !topReport.Pass {
		os.Exit(1)
	}
}

func classifyAuthError(err error) string {
	if err == context.DeadlineExceeded {
		return "TIMEOUT"
	}
	return "AUTH_ERROR"
}
