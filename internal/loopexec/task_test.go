package loopexec

import (
	"encoding/json"
	"strings"
	"testing"
)

func validTask() Task {
	return Task{SchemaVersion: 1, ID: "pilot", Objective: "Verify media behavior", Checks: []Check{{ID: "core", Kind: "go-test", Packages: []string{"./core"}}}}
}

func TestDecodeTaskRejectsInvalidContracts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Task)
	}{
		{"version", func(v *Task) { v.SchemaVersion = 2 }},
		{"path-id", func(v *Task) { v.ID = "../escape" }},
		{"empty-objective", func(v *Task) { v.Objective = "" }},
		{"empty-checks", func(v *Task) { v.Checks = nil }},
		{"duplicate-id", func(v *Task) { v.Checks = append(v.Checks, v.Checks[0]) }},
		{"kind", func(v *Task) { v.Checks[0].Kind = "shell" }},
		{"package-flag", func(v *Task) { v.Checks[0].Packages = []string{"-short"} }},
		{"regex", func(v *Task) { v.Checks[0].Run = "[" }},
		{"timeout", func(v *Task) { v.Checks[0].Timeout = "-1s" }},
		{"command-with-tests", func(v *Task) { v.Checks[0].Kind = "command"; v.Checks[0].Argv = []string{"true"} }},
		{"empty-command", func(v *Task) { v.Checks[0] = Check{ID: "cmd", Kind: "command"} }},
		{"empty-required-test", func(v *Task) { v.Checks[0].RequiredTests = []TestRef{{Package: "p"}} }},
		{"required-and-allowed", func(v *Task) {
			r := TestRef{"p", "TestOne"}
			v.Checks[0].RequiredTests = []TestRef{r}
			v.Checks[0].AllowSkips = []TestRef{r}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := validTask()
			tc.mutate(&task)
			data, _ := json.Marshal(task)
			if _, err := DecodeTask(strings.NewReader(string(data))); err == nil {
				t.Fatal("accepted invalid task")
			}
		})
	}
	for _, data := range []string{
		`{"schema_version":1,"id":"a","objective":"x","checks":[],"unknown":true}`,
		`{"schema_version":1,"id":"a","objective":"x","checks":[]} {}`,
	} {
		if _, err := DecodeTask(strings.NewReader(data)); err == nil {
			t.Fatal("accepted unknown or trailing JSON")
		}
	}
}

func TestDecodeTaskAcceptsBoundedCommandsAndTests(t *testing.T) {
	task := validTask()
	task.Checks = append(task.Checks, Check{ID: "docs", Kind: "command", Argv: []string{"bash", "tools/check-agent-docs_test.sh"}, Timeout: "20s"})
	data, _ := json.Marshal(task)
	got, err := DecodeTask(strings.NewReader(string(data)))
	if err != nil || len(got.Checks) != 2 {
		t.Fatalf("decode: %+v, %v", got, err)
	}
}
