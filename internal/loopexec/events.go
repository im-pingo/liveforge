package loopexec

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

type TestOutcome struct {
	Package string `json:"package"`
	Test    string `json:"test"`
	Status  string `json:"status"`
}

type TestSummary struct {
	Passed      int           `json:"passed"`
	Failed      int           `json:"failed"`
	Skipped     int           `json:"skipped"`
	Packages    int           `json:"packages"`
	BuildFailed bool          `json:"build_failed,omitempty"`
	Outcomes    []TestOutcome `json:"outcomes,omitempty"`
}

var errTestFailed = errors.New("tests failed")

func parseTestEvents(r io.Reader, check Check) (TestSummary, error) {
	summary := TestSummary{}
	tests := make(map[TestRef]string)
	packages := make(map[string]string)
	allowed := make(map[TestRef]bool)
	for _, ref := range check.AllowSkips {
		allowed[ref] = true
	}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	var invalid []string
	for scanner.Scan() {
		var event struct{ Action, Package, Test string }
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return summary, fmt.Errorf("invalid go test JSON: %w", err)
		}
		if len(tests)+len(packages) > 100000 {
			return summary, errors.New("test event identity limit exceeded")
		}
		if event.Action == "output" || event.Action == "build-output" || event.Action == "build-start" {
			continue
		}
		if event.Action == "build-fail" {
			summary.BuildFailed = true
			continue
		}
		switch event.Action {
		case "start", "run", "pause", "cont", "pass", "fail", "skip":
		default:
			return summary, fmt.Errorf("unknown test event action %q", event.Action)
		}
		if event.Package == "" {
			return summary, errors.New("test event has no package")
		}
		if event.Test == "" {
			if event.Action == "start" {
				if _, exists := packages[event.Package]; exists {
					invalid = append(invalid, "duplicate package start")
				}
				packages[event.Package] = "running"
			} else if event.Action == "pass" || event.Action == "fail" || event.Action == "skip" {
				if packages[event.Package] != "running" {
					invalid = append(invalid, "package terminal without unique start")
				}
				packages[event.Package] = event.Action
			} else {
				invalid = append(invalid, "unexpected package event")
			}
			continue
		}
		ref := TestRef{event.Package, event.Test}
		if packages[event.Package] != "running" {
			invalid = append(invalid, fmt.Sprintf("test outside running package: %s/%s", ref.Package, ref.Test))
		}
		switch event.Action {
		case "run":
			if _, exists := tests[ref]; exists {
				invalid = append(invalid, "duplicate test run")
			}
			tests[ref] = "running"
		case "pause", "cont":
			if tests[ref] != "running" {
				invalid = append(invalid, "test resumed without run")
			}
		case "pass", "fail", "skip":
			if tests[ref] != "running" {
				invalid = append(invalid, "test terminal without unique run")
			}
			tests[ref] = event.Action
		default:
			invalid = append(invalid, "unexpected test event")
		}
	}
	if err := scanner.Err(); err != nil {
		return summary, fmt.Errorf("read go test events: %w", err)
	}
	for ref, status := range tests {
		summary.Outcomes = append(summary.Outcomes, TestOutcome{ref.Package, ref.Test, status})
		switch status {
		case "pass":
			summary.Passed++
		case "fail":
			summary.Failed++
		case "skip":
			summary.Skipped++
			if !allowed[ref] {
				invalid = append(invalid, fmt.Sprintf("unapproved skip: %s/%s", ref.Package, ref.Test))
			}
		default:
			invalid = append(invalid, fmt.Sprintf("unfinished test: %s/%s", ref.Package, ref.Test))
		}
	}
	sort.Slice(summary.Outcomes, func(i, j int) bool {
		a, b := summary.Outcomes[i], summary.Outcomes[j]
		if a.Package != b.Package {
			return a.Package < b.Package
		}
		return a.Test < b.Test
	})
	packageFailed := false
	for pkg, status := range packages {
		if status == "running" {
			invalid = append(invalid, fmt.Sprintf("unfinished package %s", pkg))
		}
		if status == "fail" {
			packageFailed = true
		}
		if status == "pass" {
			summary.Packages++
		}
	}
	if summary.Failed > 0 || packageFailed || summary.BuildFailed {
		return summary, errTestFailed
	}
	if len(invalid) > 0 {
		sort.Strings(invalid)
		return summary, errors.New(strings.Join(invalid, "; "))
	}
	if summary.Passed == 0 || summary.Packages == 0 {
		return summary, errors.New("no passing tests and packages were executed")
	}
	for _, ref := range check.RequiredTests {
		if tests[ref] != "pass" {
			return summary, fmt.Errorf("required test did not pass: %s/%s", ref.Package, ref.Test)
		}
	}
	return summary, nil
}
