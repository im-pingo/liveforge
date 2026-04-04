package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/im-pingo/liveforge/tools/testkit/cluster"
	"github.com/im-pingo/liveforge/tools/testkit/report"
)

func runCluster(args []string) {
	fs := flag.NewFlagSet("cluster", flag.ExitOnError)

	topology := fs.String("topology", "origin-edge", "Topology: origin-edge, origin-multi-edge, origin-center-edge")
	pushProtocol := fs.String("push-protocol", "rtmp", "Protocol for pushing to origin")
	playProtocol := fs.String("play-protocol", "rtmp", "Protocol for playing from edge")
	relayProtocol := fs.String("relay-protocol", "rtmp", "Protocol for cluster relay links")
	stream := fs.String("stream", "live/test", "Stream key")
	duration := fs.Duration("duration", 5*time.Second, "Play duration (e.g. 5s, 1m)")
	edges := fs.Int("edges", 1, "Number of edge nodes (only for origin-multi-edge)")
	binary := fs.String("binary", "", "Path to liveforge binary (default: auto-detect)")
	output := fs.String("output", "", "Output format: human, json (default: auto-detect from TTY)")
	timeout := fs.Duration("timeout", 120*time.Second, "Overall timeout (e.g. 120s, 5m)")

	var asserts stringSlice
	fs.Var(&asserts, "assert", "Assertion expression (repeatable, e.g. 'cluster.relay_ms<500')")

	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	// Resolve liveforge binary.
	binaryPath := *binary
	if binaryPath == "" {
		var err error
		binaryPath, err = findLiveforBinary()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			fmt.Fprintln(os.Stderr, "Hint: set --binary or $LF_BINARY, or build to ./bin/liveforge")
			os.Exit(2)
		}
	}

	// Build topology.
	var topo *cluster.Topology
	switch *topology {
	case "origin-edge":
		topo = cluster.OriginEdge(*relayProtocol)
	case "origin-multi-edge":
		topo = cluster.OriginMultiEdge(*relayProtocol, *edges)
	case "origin-center-edge":
		topo = cluster.OriginCenterEdge(*relayProtocol)
	default:
		fmt.Fprintf(os.Stderr, "Error: unknown topology %q\n", *topology)
		fmt.Fprintln(os.Stderr, "Available: origin-edge, origin-multi-edge, origin-center-edge")
		os.Exit(2)
	}

	cfg := cluster.ClusterTestConfig{
		BinaryPath:   binaryPath,
		Topology:     topo,
		PushProtocol: *pushProtocol,
		PlayProtocol: *playProtocol,
		StreamKey:    *stream,
		Duration:     *duration,
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	start := time.Now()
	orchestrator := cluster.NewOrchestrator(binaryPath)
	clusterReport, err := orchestrator.Run(ctx, cfg)
	elapsed := time.Since(start)

	topReport := &report.TopLevelReport{
		Command:    "cluster",
		Timestamp:  start,
		DurationMs: elapsed.Milliseconds(),
		Pass:       true,
		Cluster:    clusterReport,
	}

	if err != nil {
		topReport.Pass = false
		topReport.Errors = append(topReport.Errors, report.ErrorDetail{
			Code:    classifyClusterError(err),
			Message: err.Error(),
		})
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

// findLiveforBinary locates the liveforge binary using the same search order
// as the cluster package's test helper: $LF_BINARY, ./bin/liveforge,
// $GOPATH/bin/liveforge.
func findLiveforBinary() (string, error) {
	// Check environment variable.
	if bin := os.Getenv("LF_BINARY"); bin != "" {
		if _, err := os.Stat(bin); err == nil {
			return bin, nil
		}
	}

	// Check ./bin/liveforge.
	if bin, err := filepath.Abs("bin/liveforge"); err == nil {
		if _, err := os.Stat(bin); err == nil {
			return bin, nil
		}
	}

	// Check $GOPATH/bin/liveforge.
	gopath := os.Getenv("GOPATH")
	if gopath == "" {
		home, err := os.UserHomeDir()
		if err == nil {
			gopath = filepath.Join(home, "go")
		}
	}
	if gopath != "" {
		bin := filepath.Join(gopath, "bin", "liveforge")
		if _, err := os.Stat(bin); err == nil {
			return bin, nil
		}
	}

	return "", fmt.Errorf("liveforge binary not found")
}

func classifyClusterError(err error) string {
	if err == context.DeadlineExceeded {
		return "TIMEOUT"
	}
	return "CLUSTER_ERROR"
}
