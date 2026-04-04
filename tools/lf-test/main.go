// Package main implements the lf-test CLI, a streaming server integration
// testing tool. It dispatches to subcommands (push, play, auth, cluster)
// using the standard library flag package.
package main

import (
	"fmt"
	"os"
	"strings"
)

// stringSlice implements flag.Value for repeatable string flags (e.g. --assert).
type stringSlice []string

func (s *stringSlice) String() string { return strings.Join(*s, ", ") }
func (s *stringSlice) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// isTerminal reports whether stdout is connected to a terminal.
func isTerminal() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// outputFormat returns "human" or "json" based on the --output flag value.
// An empty string triggers auto-detection from TTY state.
func outputFormat(flag string) string {
	switch flag {
	case "human":
		return "human"
	case "json":
		return "json"
	default:
		if isTerminal() {
			return "human"
		}
		return "json"
	}
}

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "push":
		runPush(os.Args[2:])
	case "play":
		runPlay(os.Args[2:])
	case "auth":
		runAuth(os.Args[2:])
	case "cluster":
		runCluster(os.Args[2:])
	case "help", "--help", "-h":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `lf-test - LiveForge streaming server integration testing tool

Usage: lf-test <command> [flags]

Commands:
  push      Push media to a streaming server
  play      Subscribe and analyze a media stream
  auth      Run authentication test matrix
  cluster   Run multi-node cluster test

Global flags apply to all commands:
  --output   Output format: human, json (default: auto-detect from TTY)
  --timeout  Overall timeout (e.g. 30s, 1m)

Run 'lf-test <command> --help' for command-specific flags.
`)
}
