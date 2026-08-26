package protocoltest

import (
	"testing"
	"time"
)

func TestNewReportRequiresEveryCheckToPass(t *testing.T) {
	tests := []struct {
		name     string
		checks   []Check
		wantPass bool
	}{
		{name: "all checks pass", checks: []Check{{Name: "rtp", Passed: true}}, wantPass: true},
		{name: "one check fails", checks: []Check{{Name: "rtp", Passed: true}, {Name: "rtcp", Passed: false}}, wantPass: false},
		{name: "no checks", checks: nil, wantPass: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report := New("test", tt.checks)
			if report.Passed != tt.wantPass {
				t.Fatalf("Passed=%v, want %v", report.Passed, tt.wantPass)
			}
			if report.Protocol != "test" || report.RanAt.IsZero() {
				t.Fatalf("report metadata = %+v", report)
			}
		})
	}
}

func TestNewWithDurationPublishesMeasuredDuration(t *testing.T) {
	report := NewWithDuration("sip", []Check{{Name: "register", Passed: true}}, 37*time.Millisecond)
	if report.DurationMS != 37 {
		t.Fatalf("duration_ms=%d, want 37", report.DurationMS)
	}
}
