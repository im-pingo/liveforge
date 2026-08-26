package sipgateway

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	sipmod "github.com/im-pingo/liveforge/module/sip"
	"github.com/im-pingo/liveforge/pkg/avframe"
)

func validSIPLabRequest() LabSessionRequest {
	return LabSessionRequest{
		Mode:      LabModePublish,
		DeviceID:  "sip-lab-device",
		StreamKey: "sip/lab",
		Codec:     "PCMA",
	}
}

func TestLabManagerRejectsInvalidStartRequest(t *testing.T) {
	manager := NewLabManager()

	_, err := manager.Start(context.Background(), LabSessionRequest{})
	if !errors.Is(err, ErrLabInvalidRequest) {
		t.Fatalf("Start error = %v, want ErrLabInvalidRequest", err)
	}
}

func TestLabManagerListsStartedSession(t *testing.T) {
	manager := NewLabManager()
	want := validSIPLabRequest()

	session, err := manager.Start(context.Background(), want)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	listed := manager.List()
	if len(listed) != 1 || listed[0] != session {
		t.Fatalf("List = %+v, want [%+v]", listed, session)
	}
	if listed[0].DeviceID != want.DeviceID || listed[0].StreamKey != want.StreamKey {
		t.Fatalf("listed session identity = %+v, want device=%q stream=%q", listed[0], want.DeviceID, want.StreamKey)
	}
}

func TestLabManagerRejectsDuplicateIdentity(t *testing.T) {
	manager := NewLabManager()
	want := validSIPLabRequest()
	if _, err := manager.Start(context.Background(), want); err != nil {
		t.Fatalf("first Start: %v", err)
	}

	_, err := manager.Start(context.Background(), want)
	if !errors.Is(err, ErrLabDuplicateIdentity) {
		t.Fatalf("duplicate Start error = %v, want ErrLabDuplicateIdentity", err)
	}
}

func TestLabManagerStopIsIdempotent(t *testing.T) {
	manager := NewLabManager()
	session, err := manager.Start(context.Background(), validSIPLabRequest())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := manager.Stop(session.ID); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if err := manager.Stop(session.ID); err != nil {
		t.Fatalf("second Stop: %v, want nil", err)
	}
}

func TestSIPLabPublishUsesRealSignalingAndCleansUp(t *testing.T) {
	h := newRealSIPLabHarness(t)
	request := LabSessionRequest{
		Mode:      LabModePublish,
		DeviceID:  "persistent-publish-device",
		StreamKey: "sip/persistent-publish",
		Codec:     "PCMA",
	}

	session, err := h.module.StartLabSession(context.Background(), request)
	if err != nil {
		t.Fatalf("StartLabSession publish: %v", err)
	}
	active := waitForSIPLabSnapshot(t, h.module, session.ID, func(snapshot LabSessionSnapshot) bool {
		return snapshot.State == LabSessionStateActive &&
			snapshot.RTPPacketsSent > 0 && snapshot.RTCPPacketsSent > 0
	})
	if active.Direction != LabDirectionInbound || active.Codec != "PCMA" {
		t.Fatalf("publish snapshot = %+v, want inbound PCMA", active)
	}
	stream, ok := h.hub.Find(request.StreamKey)
	if !ok || stream.Publisher() == nil {
		t.Fatalf("publish stream = (%v, %v), want active real publisher", stream, ok)
	}

	if err := h.module.StopLabSession(session.ID); err != nil {
		t.Fatalf("StopLabSession publish: %v", err)
	}
	if err := h.module.StopLabSession(session.ID); err != nil {
		t.Fatalf("second StopLabSession publish: %v", err)
	}
	waitForSIPLab(t, func() bool {
		stream, ok := h.hub.Find(request.StreamKey)
		return h.module.Gateway().ActiveCalls() == 0 && ok && stream.Publisher() == nil
	})
	if _, err := h.module.StartLabSession(context.Background(), request); err != nil {
		t.Fatalf("reusing stopped publish identity: %v", err)
	}
}

func TestSIPLabReceiveAcceptsGatewayInviteAndCleansUp(t *testing.T) {
	h := newRealSIPLabHarness(t)
	stream, err := h.hub.GetOrCreate("sip/persistent-receive")
	if err != nil {
		t.Fatalf("GetOrCreate receive stream: %v", err)
	}
	publishTestAudio(t, stream, avframe.CodecG711U)
	request := LabSessionRequest{
		Mode:      LabModeReceive,
		DeviceID:  "persistent-receive-device",
		StreamKey: "sip/persistent-receive",
		Codec:     "PCMU",
	}

	session, err := h.module.StartLabSession(context.Background(), request)
	if err != nil {
		t.Fatalf("StartLabSession receive: %v", err)
	}
	active := waitForSIPLabSnapshot(t, h.module, session.ID, func(snapshot LabSessionSnapshot) bool {
		return snapshot.State == LabSessionStateActive &&
			snapshot.RTPPacketsRecv > 0 && snapshot.RTCPPacketsRecv > 0
	})
	if active.Direction != LabDirectionOutbound || active.Codec != "PCMU" {
		t.Fatalf("receive snapshot = %+v, want outbound PCMU", active)
	}

	if err := h.module.StopLabSession(session.ID); err != nil {
		t.Fatalf("StopLabSession receive: %v", err)
	}
	if err := h.module.StopLabSession(session.ID); err != nil {
		t.Fatalf("second StopLabSession receive: %v", err)
	}
	waitForSIPLab(t, func() bool { return h.module.Gateway().ActiveCalls() == 0 })
}

func TestSIPLabRejectsInvalidAndDuplicateIdentities(t *testing.T) {
	h := newRealSIPLabHarness(t)
	request := LabSessionRequest{Mode: LabModePublish, DeviceID: "duplicate-device", StreamKey: "sip/duplicate"}
	if _, err := h.module.StartLabSession(context.Background(), request); !errors.Is(err, ErrLabInvalidRequest) {
		t.Fatalf("invalid codec StartLabSession error = %v, want ErrLabInvalidRequest", err)
	}
	request.Codec = "PCMA"
	if _, err := h.module.StartLabSession(context.Background(), request); err != nil {
		t.Fatalf("first duplicate StartLabSession: %v", err)
	}
	if _, err := h.module.StartLabSession(context.Background(), request); !errors.Is(err, ErrLabDuplicateIdentity) {
		t.Fatalf("duplicate StartLabSession error = %v, want ErrLabDuplicateIdentity", err)
	}
}

type realSIPLabHarness struct {
	module *Module
	hub    *core.StreamHub
}

func newRealSIPLabHarness(t *testing.T) realSIPLabHarness {
	t.Helper()
	sipAddr := freeSIPLabUDPAddress(t)
	cfg := &config.Config{
		SIP: config.SIPConfig{
			Enabled:   true,
			Listen:    sipAddr,
			Transport: []string{"udp"},
			ServerID:  "liveforge-lab",
			Domain:    "lab.local",
			Gateway: config.SIPGatewayConfig{
				Enabled:      true,
				StreamPrefix: "sip",
				RTPPortRange: []int{evenSIPLabPort(t), 0},
				Codecs:       []string{"PCMA", "PCMU"},
				MaxCalls:     8,
			},
		},
		Stream: config.StreamConfig{RingBufferSize: 256},
	}
	cfg.SIP.Gateway.RTPPortRange[1] = cfg.SIP.Gateway.RTPPortRange[0] + 100
	server := core.NewServer(cfg)
	sipModule := sipmod.NewModule()
	gatewayModule := NewModule(sipModule.Service())
	server.RegisterModule(sipModule)
	server.RegisterModule(gatewayModule)
	if err := server.Init(); err != nil {
		t.Fatalf("Init real SIP lab harness: %v", err)
	}
	t.Cleanup(server.Shutdown)
	time.Sleep(25 * time.Millisecond)
	return realSIPLabHarness{module: gatewayModule, hub: server.StreamHub()}
}

func freeSIPLabUDPAddress(t *testing.T) string {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("reserve SIP address: %v", err)
	}
	defer conn.Close()
	return conn.LocalAddr().String()
}

func evenSIPLabPort(t *testing.T) int {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("reserve RTP port: %v", err)
	}
	defer conn.Close()
	port := conn.LocalAddr().(*net.UDPAddr).Port
	if port%2 != 0 {
		port++
	}
	return port
}

func waitForSIPLabSnapshot(t *testing.T, module SIPGatewayProvider, id string, predicate func(LabSessionSnapshot) bool) LabSessionSnapshot {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, snapshot := range module.ListLabSessions() {
			if snapshot.ID == id && predicate(snapshot) {
				return snapshot
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("lab session %q did not reach expected state; snapshots=%+v", id, module.ListLabSessions())
	return LabSessionSnapshot{}
}

func waitForSIPLab(t *testing.T, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("SIP lab condition was not reached")
}
