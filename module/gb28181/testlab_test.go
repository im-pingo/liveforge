package gb28181

import (
	"context"
	"net"
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
	pair, err := ports.AllocateBoundUDPPair("udp4", net.ParseIP("127.0.0.1"))
	if err != nil {
		t.Fatalf("successful self-test did not close/free its port pair: %v", err)
	}
	closeBoundUDPPair(pair)
	ports.Free(pair.RTPPort, pair.RTCPPort)
}

func TestModuleSelfTestFailsPortCheckWhenConfiguredPairIsExternallyOccupied(t *testing.T) {
	portRange := freeGBLabRTPPortRange(t, 1)
	loopback := net.ParseIP("127.0.0.1")
	rtpConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: loopback, Port: portRange[0]})
	if err != nil {
		t.Fatal(err)
	}
	defer rtpConn.Close()
	rtcpConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: loopback, Port: portRange[1]})
	if err != nil {
		t.Fatal(err)
	}
	defer rtcpConn.Close()
	ports, err := portalloc.New(portRange[0], portRange[1])
	if err != nil {
		t.Fatal(err)
	}

	report, err := (&Module{handler: &handler{ports: ports}}).RunSelfTest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range report.Checks {
		if check.Name == "rtp_port_allocation" {
			if check.Passed {
				t.Fatalf("occupied GB28181 port check passed: %+v", check)
			}
			return
		}
	}
	t.Fatalf("self-test omitted rtp_port_allocation: %+v", report.Checks)
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
