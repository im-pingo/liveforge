package loopexec

import (
	"strings"
	"testing"
)

func TestEventOracleRejectsFalseSuccess(t *testing.T) {
	passed := "{\"Action\":\"start\",\"Package\":\"p\"}\n{\"Action\":\"run\",\"Package\":\"p\",\"Test\":\"TestOne\"}\n{\"Action\":\"pass\",\"Package\":\"p\",\"Test\":\"TestOne\"}\n{\"Action\":\"pass\",\"Package\":\"p\"}\n"
	for _, tc := range []struct {
		name, data string
		check      Check
	}{
		{"empty", "", Check{}},
		{"no-tests", "{\"Action\":\"start\",\"Package\":\"p\"}\n{\"Action\":\"pass\",\"Package\":\"p\"}\n", Check{}},
		{"malformed", passed + "not json\n", Check{}},
		{"unfinished-package", strings.TrimSuffix(passed, "{\"Action\":\"pass\",\"Package\":\"p\"}\n"), Check{}},
		{"unfinished-test", strings.ReplaceAll(passed, "{\"Action\":\"pass\",\"Package\":\"p\",\"Test\":\"TestOne\"}\n", ""), Check{}},
		{"failed-test", strings.Replace(passed, "\"Test\":\"TestOne\"}\n{\"Action\":\"pass\"", "\"Test\":\"TestOne\"}\n{\"Action\":\"fail\"", 1), Check{}},
		{"skipped", strings.Replace(passed, "{\"Action\":\"pass\",\"Package\":\"p\",\"Test\"", "{\"Action\":\"skip\",\"Package\":\"p\",\"Test\"", 1), Check{}},
		{"required-missing", passed, Check{RequiredTests: []TestRef{{"p", "TestOther"}}}},
		{"duplicate-terminal", passed + "{\"Action\":\"pass\",\"Package\":\"p\",\"Test\":\"TestOne\"}\n", Check{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseTestEvents(strings.NewReader(tc.data), tc.check); err == nil {
				t.Fatal("false passing evidence")
			}
		})
	}
}

func TestEventOracleCountsAndAllowsOnlyExplicitSkips(t *testing.T) {
	data := "{\"Action\":\"start\",\"Package\":\"p\"}\n{\"Action\":\"run\",\"Package\":\"p\",\"Test\":\"TestOne\"}\n{\"Action\":\"pass\",\"Package\":\"p\",\"Test\":\"TestOne\"}\n{\"Action\":\"run\",\"Package\":\"p\",\"Test\":\"TestOptional\"}\n{\"Action\":\"skip\",\"Package\":\"p\",\"Test\":\"TestOptional\"}\n{\"Action\":\"pass\",\"Package\":\"p\"}\n"
	check := Check{RequiredTests: []TestRef{{"p", "TestOne"}}, AllowSkips: []TestRef{{"p", "TestOptional"}}}
	got, err := parseTestEvents(strings.NewReader(data), check)
	if err != nil || got.Passed != 1 || got.Skipped != 1 || got.Packages != 1 {
		t.Fatalf("summary=%+v err=%v", got, err)
	}
	check.AllowSkips = []TestRef{{"other", "TestOptional"}}
	if _, err := parseTestEvents(strings.NewReader(data), check); err == nil {
		t.Fatal("skip allowance escaped package scope")
	}
}

func TestEventOracleRequiresTestsInsideRunningPackage(t *testing.T) {
	for _, data := range []string{
		"{\"Action\":\"start\",\"Package\":\"other\"}\n{\"Action\":\"pass\",\"Package\":\"other\"}\n{\"Action\":\"run\",\"Package\":\"p\",\"Test\":\"TestOne\"}\n{\"Action\":\"pass\",\"Package\":\"p\",\"Test\":\"TestOne\"}\n",
		"{\"Action\":\"start\",\"Package\":\"p\"}\n{\"Action\":\"pass\",\"Package\":\"p\"}\n{\"Action\":\"run\",\"Package\":\"p\",\"Test\":\"TestOne\"}\n{\"Action\":\"pass\",\"Package\":\"p\",\"Test\":\"TestOne\"}\n",
	} {
		if _, err := parseTestEvents(strings.NewReader(data), Check{RequiredTests: []TestRef{{"p", "TestOne"}}}); err == nil {
			t.Fatal("accepted test outside package lifetime")
		}
	}
}
func TestEventOracleReportsEveryUnapprovedSkipDeterministically(t *testing.T) {
	data := "{\"Action\":\"start\",\"Package\":\"p\"}\n"
	for _, name := range []string{"TestPass", "TestB", "TestA"} {
		status := "skip"
		if name == "TestPass" {
			status = "pass"
		}
		data += "{\"Action\":\"run\",\"Package\":\"p\",\"Test\":\"" + name + "\"}\n"
		data += "{\"Action\":\"" + status + "\",\"Package\":\"p\",\"Test\":\"" + name + "\"}\n"
	}
	data += "{\"Action\":\"pass\",\"Package\":\"p\"}\n"
	_, err := parseTestEvents(strings.NewReader(data), Check{})
	if err == nil || err.Error() != "unapproved skip: p/TestA; unapproved skip: p/TestB" {
		t.Fatalf("incomplete or unstable reason: %v", err)
	}
}
