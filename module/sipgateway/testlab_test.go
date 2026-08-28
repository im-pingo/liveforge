package sipgateway

import (
	"context"
	"net"
	"strings"
	"testing"

	"github.com/im-pingo/liveforge/pkg/portalloc"
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
	pair, err := gw.portAlloc.AllocateBoundUDPPair("udp4", net.ParseIP("127.0.0.1"))
	if err != nil {
		t.Fatalf("successful self-test did not close/free its port pair: %v", err)
	}
	_ = pair.RTPConn.Close()
	_ = pair.RTCPConn.Close()
	gw.portAlloc.Free(pair.RTPPort, pair.RTCPPort)
}

func TestGatewaySelfTestFailsPortCheckWhenConfiguredPairIsExternallyOccupied(t *testing.T) {
	rtpConn, rtcpConn, start := reserveSIPSelfTestPair(t)
	defer rtpConn.Close()
	defer rtcpConn.Close()
	ports, err := portalloc.New(start, start+1)
	if err != nil {
		t.Fatal(err)
	}
	gw := &Gateway{portAlloc: ports, codecs: []string{"PCMA", "PCMU"}}

	report := gw.RunSelfTest(context.Background())
	for _, check := range report.Checks {
		if check.Name == "rtp_port_allocation" {
			if check.Passed {
				t.Fatalf("occupied SIP port check passed: %+v", check)
			}
			return
		}
	}
	t.Fatalf("self-test omitted rtp_port_allocation: %+v", report.Checks)
}

func reserveSIPSelfTestPair(t *testing.T) (*net.UDPConn, *net.UDPConn, int) {
	t.Helper()
	loopback := net.ParseIP("127.0.0.1")
	for range 128 {
		probe, err := net.ListenUDP("udp4", &net.UDPAddr{IP: loopback})
		if err != nil {
			t.Fatal(err)
		}
		start := probe.LocalAddr().(*net.UDPAddr).Port
		_ = probe.Close()
		if start%2 != 0 {
			start--
		}
		if start < 1024 || start >= 65535 {
			continue
		}
		rtpConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: loopback, Port: start})
		if err != nil {
			continue
		}
		rtcpConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: loopback, Port: start + 1})
		if err == nil {
			return rtpConn, rtcpConn, start
		}
		_ = rtpConn.Close()
	}
	t.Fatal("could not reserve a consecutive UDP pair")
	return nil, nil, 0
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
