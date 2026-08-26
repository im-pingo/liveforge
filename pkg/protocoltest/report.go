package protocoltest

import "time"

// Check is one deterministic protocol self-test assertion.
type Check struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail,omitempty"`
}

// Report is the stable result rendered by the management console.
type Report struct {
	Protocol   string    `json:"protocol"`
	Passed     bool      `json:"passed"`
	RanAt      time.Time `json:"ran_at"`
	DurationMS int64     `json:"duration_ms"`
	Checks     []Check   `json:"checks"`
}

func New(protocol string, checks []Check) Report {
	return NewWithDuration(protocol, checks, 0)
}

// NewWithDuration creates a report with an explicit execution duration so a
// UI can distinguish a completed local protocol run from a stale result.
func NewWithDuration(protocol string, checks []Check, duration time.Duration) Report {
	passed := true
	for _, check := range checks {
		if !check.Passed {
			passed = false
			break
		}
	}
	return Report{Protocol: protocol, Passed: passed, RanAt: time.Now().UTC(), DurationMS: duration.Milliseconds(), Checks: checks}
}
