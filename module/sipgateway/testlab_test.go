package sipgateway

import (
	"context"
	"strings"
	"testing"
)

func TestGatewaySelfTestRunsWithoutRemotePeer(t *testing.T) {
	gw, _, _ := newControlPlaneGateway(t, newTestGatewayConfig(t))
	report := gw.RunSelfTest(context.Background())
	if !report.Passed {
		t.Fatalf("self-test failed: %+v", report)
	}
	if report.DurationMS <= 0 {
		t.Fatalf("self-test duration_ms=%d, want measured duration", report.DurationMS)
	}
	if len(report.Checks) < 3 {
		t.Fatalf("self-test checks = %+v", report.Checks)
	}
}

func TestGatewaySelfTestCoversLocalSignalingAndMediaLifecycle(t *testing.T) {
	gw, _, _ := newControlPlaneGateway(t, newTestGatewayConfig(t))
	report := gw.RunSelfTest(context.Background())
	if !report.Passed {
		t.Fatalf("self-test failed: %+v", report)
	}
	for _, name := range []string{
		"register_401_challenge",
		"register_authenticated",
		"invite_200_ok",
		"ack",
		"rtp_media_loopback",
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
			t.Fatalf("missing passed SIP lifecycle check %q in %+v", name, report.Checks)
		}
	}
}
