// lf-loop executes trusted local verification tasks and writes evidence reports.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/im-pingo/liveforge/internal/loopexec"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := runCLI(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func runCLI(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	const usage = "Usage: lf-loop run -task TASK.json [-profile portable|tagged] [-root DIR] [-out DIR] [-timeout 30m] [-json]"
	if len(args) == 1 && (args[0] == "help" || args[0] == "-h" || args[0] == "--help") {
		_, _ = fmt.Fprintln(stdout, usage)
		return 0
	}
	if len(args) == 0 || args[0] != "run" {
		_, _ = fmt.Fprintln(stderr, usage)
		return 2
	}
	flags := flag.NewFlagSet("lf-loop run", flag.ContinueOnError)
	flags.SetOutput(stderr)
	taskPath := flags.String("task", "", "trusted local JSON task")
	profile := flags.String("profile", "portable", "portable (no CGO) or tagged (audiocodec and race)")
	root := flags.String("root", ".", "repository directory; checks run at its Git root")
	output := flags.String("out", "", "ignored or external report directory (default .lf-loop/runs)")
	timeout := flags.Duration("timeout", 30*time.Minute, "complete task budget, including lock wait")
	asJSON := flags.Bool("json", false, "write the final JSON report to stdout")
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *taskPath == "" || flags.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, usage)
		return 2
	}
	if *timeout <= 0 || *timeout > 24*time.Hour {
		_, _ = fmt.Fprintln(stderr, "timeout must be positive and at most 24h")
		return 2
	}
	file, err := os.Open(*taskPath)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	task, err := loopexec.DecodeTask(file)
	_ = file.Close()
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "invalid task:", err)
		return 2
	}
	report, err := loopexec.Run(ctx, *root, task, loopexec.Options{Profile: *profile, Output: *output, Timeout: *timeout})
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "executor:", err)
	}
	if *asJSON {
		if writeErr := json.NewEncoder(stdout).Encode(report); writeErr != nil {
			_, _ = fmt.Fprintln(stderr, writeErr)
			return 2
		}
	} else {
		_, _ = fmt.Fprintf(stdout, "%s: %s\n", task.ID, report.Status)
		for _, check := range report.Checks {
			_, _ = fmt.Fprintf(stdout, "  %s: %s", check.ID, check.Status)
			if check.Reason != "" {
				_, _ = fmt.Fprintf(stdout, " (%s)", check.Reason)
			}
			_, _ = fmt.Fprintln(stdout)
		}
		if report.Reason != "" {
			_, _ = fmt.Fprintln(stdout, report.Reason)
		}
		if report.Directory != "" {
			_, _ = fmt.Fprintln(stdout, "Report:", filepath.Join(report.Directory, "report.json"))
		}
	}
	if err != nil {
		return 2
	}
	return report.ExitCode()
}
