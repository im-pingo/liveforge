package gb28181

import (
	"context"
	"strings"
	"testing"

	"github.com/im-pingo/liveforge/pkg/portalloc"
)

func TestModuleSelfTestRunsWithoutExternalDevice(t *testing.T) {
	ports, err := portalloc.New(41000, 41020)
	if err != nil {
		t.Fatal(err)
	}
	m := &Module{handler: &handler{ports: ports}}
	report, err := m.RunSelfTest(context.Background())
	if err != nil || !report.Passed {
		t.Fatalf("self-test report=%+v err=%v", report, err)
	}
	if report.DurationMS <= 0 {
		t.Fatalf("self-test duration_ms=%d, want measured duration", report.DurationMS)
	}
	if len(report.Checks) < 3 {
		t.Fatalf("self-test checks = %+v", report.Checks)
	}
}

func TestModuleSelfTestCoversLocalGBSignalingAndMediaLifecycle(t *testing.T) {
	ports, err := portalloc.New(41000, 41020)
	if err != nil {
		t.Fatal(err)
	}
	m := &Module{handler: &handler{ports: ports}}
	report, err := m.RunSelfTest(context.Background())
	if err != nil || !report.Passed {
		t.Fatalf("self-test report=%+v err=%v", report, err)
	}
	for _, name := range []string{
		"register",
		"keepalive",
		"catalog",
		"play_invite_200_ok",
		"play_sdp_answer",
		"ps_rtp_media_loopback",
		"rtcp_control_loopback",
		"bye",
		"invite_rejection",
		"invite_timeout",
	} {
		found := false
		for _, check := range report.Checks {
			if check.Name == name && check.Passed && !strings.Contains(check.Detail, "unavailable") {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing passed GB28181 lifecycle check %q in %+v", name, report.Checks)
		}
	}
}
