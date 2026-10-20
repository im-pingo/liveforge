package loopexec

import "time"

type Options struct {
	Profile string
	Output  string
	Timeout time.Duration
}

type CheckResult struct {
	ID               string            `json:"id"`
	Kind             string            `json:"kind"`
	Status           string            `json:"status"`
	Reason           string            `json:"reason,omitempty"`
	Argv             []string          `json:"argv,omitempty"`
	Environment      map[string]string `json:"environment,omitempty"`
	StartedAt        time.Time         `json:"started_at"`
	FinishedAt       time.Time         `json:"finished_at"`
	ExitCode         int               `json:"exit_code"`
	Executable       string            `json:"executable,omitempty"`
	ExecutableSHA256 string            `json:"executable_sha256,omitempty"`
	Stdout           string            `json:"stdout,omitempty"`
	Stderr           string            `json:"stderr,omitempty"`
	Truncated        bool              `json:"truncated,omitempty"`
	Tests            *TestSummary      `json:"tests,omitempty"`
	Before           string            `json:"source_before,omitempty"`
	After            string            `json:"source_after,omitempty"`
}

type Report struct {
	Inputs        []string          `json:"inputs,omitempty"`
	SchemaVersion int               `json:"schema_version"`
	Task          Task              `json:"task"`
	TaskSHA256    string            `json:"task_sha256"`
	AttemptID     string            `json:"attempt_id"`
	Directory     string            `json:"directory"`
	Root          string            `json:"root"`
	Profile       string            `json:"profile"`
	Status        string            `json:"status"`
	Reason        string            `json:"reason,omitempty"`
	StartedAt     time.Time         `json:"started_at"`
	FinishedAt    time.Time         `json:"finished_at"`
	Before        Snapshot          `json:"source_before"`
	After         Snapshot          `json:"source_after"`
	Tools         map[string]string `json:"tools"`
	Checks        []CheckResult     `json:"checks"`
}

func (r Report) ExitCode() int {
	switch r.Status {
	case "passed":
		return 0
	case "blocked_env":
		return 3
	case "timed_out":
		return 124
	case "canceled":
		return 130
	case "error", "running":
		return 2
	default:
		return 1
	}
}
