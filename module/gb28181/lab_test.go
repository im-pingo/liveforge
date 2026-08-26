package gb28181

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

func validGBLabRequest() LabSessionRequest {
	return LabSessionRequest{
		Mode:      LabModePublish,
		DeviceID:  "34020000001320000001",
		ChannelID: "34020000001320000002",
		StreamKey: "gb28181/lab",
	}
}

func TestLabManagerRejectsInvalidStartRequest(t *testing.T) {
	manager := NewLabManager()

	_, err := manager.Start(context.Background(), LabSessionRequest{})
	if !errors.Is(err, ErrLabInvalidRequest) {
		t.Fatalf("Start error = %v, want ErrLabInvalidRequest", err)
	}
}

func TestLabManagerRejectsValidStartAsUnavailable(t *testing.T) {
	manager := NewLabManager()

	_, err := manager.Start(context.Background(), validGBLabRequest())
	if !errors.Is(err, ErrLabManagerUnimplemented) {
		t.Fatalf("valid Start error = %v, want ErrLabManagerUnimplemented", err)
	}
	if listed := manager.List(); len(listed) != 0 {
		t.Fatalf("List = %+v, want no transportless sessions", listed)
	}
}

func TestLabManagerDoesNotReserveUnavailableIdentity(t *testing.T) {
	manager := NewLabManager()
	want := validGBLabRequest()
	for attempt := 0; attempt < 2; attempt++ {
		if _, err := manager.Start(context.Background(), want); !errors.Is(err, ErrLabManagerUnimplemented) {
			t.Fatalf("Start attempt %d error = %v, want ErrLabManagerUnimplemented", attempt+1, err)
		}
	}
}

func TestLabManagerStopReportsUnavailable(t *testing.T) {
	manager := NewLabManager()
	if err := manager.Stop("unavailable"); !errors.Is(err, ErrLabManagerUnimplemented) {
		t.Fatalf("Stop error = %v, want ErrLabManagerUnimplemented", err)
	}
}

func TestGBLabPublishUsesRealRegistrationAndMedia(t *testing.T) {
	h := newRealGBLabHarness(t)
	request := LabSessionRequest{
		Mode:      LabModePublish,
		DeviceID:  "34020000001320000011",
		ChannelID: "34020000001320000012",
		StreamKey: "gb28181/34020000001320000012",
	}

	session, err := h.module.StartLabSession(context.Background(), request)
	if err != nil {
		t.Fatalf("StartLabSession publish: %v", err)
	}
	lab := h.module.labs.session(session.ID)
	if lab == nil {
		t.Fatal("publish lab session was not retained")
	}
	publishRTP, publishRTCP := lab.mediaAddresses()
	active := waitForGBLabSnapshot(t, h.module, session.ID, func(snapshot LabSessionSnapshot) bool {
		return snapshot.State == LabSessionStateActive &&
			snapshot.RTPPacketsSent > 0 && snapshot.PSFramesSent > 0
	})
	if active.Direction != LabDirectionInbound || active.ChannelID != request.ChannelID {
		t.Fatalf("publish snapshot = %+v, want inbound channel %q", active, request.ChannelID)
	}
	waitForGBLab(t, func() bool {
		device := h.module.registry.Get(request.DeviceID)
		return device != nil && device.Status == DeviceStatusOnline && device.Channels[request.ChannelID] != nil
	})
	stream, ok := h.hub.Find(request.StreamKey)
	if !ok || stream.Publisher() == nil {
		t.Fatalf("publish stream = (%v, %v), keys=%v, want active publisher", stream, ok, h.hub.Keys())
	}

	if err := h.module.StopLabSession(session.ID); err != nil {
		t.Fatalf("StopLabSession publish: %v", err)
	}
	if err := h.module.StopLabSession(session.ID); err != nil {
		t.Fatalf("second StopLabSession publish: %v", err)
	}
	waitForGBLab(t, func() bool {
		return len(h.module.sessions.All()) == 0 && stream.Publisher() == nil && h.module.registry.Get(request.DeviceID) == nil
	})
	assertGBLabPortFree(t, publishRTP)
	assertGBLabPortFree(t, publishRTCP)
}

func TestGBLabPublishUsesRequestedCustomStreamKey(t *testing.T) {
	h := newRealGBLabHarness(t)
	request := LabSessionRequest{
		Mode:      LabModePublish,
		DeviceID:  "34020000001320000041",
		ChannelID: "34020000001320000042",
		StreamKey: "lab/custom-gb-stream",
	}

	session, err := h.module.StartLabSession(context.Background(), request)
	if err != nil {
		t.Fatalf("StartLabSession publish: %v", err)
	}
	t.Cleanup(func() { _ = h.module.StopLabSession(session.ID) })

	waitForGBLabSnapshot(t, h.module, session.ID, func(snapshot LabSessionSnapshot) bool {
		return snapshot.State == LabSessionStateActive && snapshot.RTPPacketsSent > 0
	})
	customStream, customOK := h.hub.Find(request.StreamKey)
	if !customOK || customStream.Publisher() == nil {
		t.Fatalf("custom stream = (%v, %v), keys=%v, want active publisher", customStream, customOK, h.hub.Keys())
	}
	defaultKey := h.module.handler.prefix + "/" + request.ChannelID
	if defaultStream, defaultOK := h.hub.Find(defaultKey); defaultOK && defaultStream.Publisher() != nil {
		t.Fatalf("default stream %q unexpectedly has an active publisher", defaultKey)
	}
}

func TestGBLabReceiveAcceptsRealLivePlayAndCountsMedia(t *testing.T) {
	h := newRealGBLabHarness(t)
	stream, err := h.hub.GetOrCreate("gb28181/source")
	if err != nil {
		t.Fatalf("GetOrCreate source: %v", err)
	}
	publisher := NewPublisher("gb28181-source", nil)
	if err := stream.SetPublisher(publisher); err != nil {
		t.Fatalf("SetPublisher source: %v", err)
	}
	frame := deterministicGBLabTestFrame(0)
	stream.WriteFrame(frame)
	request := LabSessionRequest{
		Mode:      LabModeReceive,
		DeviceID:  "34020000001320000021",
		ChannelID: "34020000001320000022",
		StreamKey: "gb28181/source",
	}

	session, err := h.module.StartLabSession(context.Background(), request)
	if err != nil {
		t.Fatalf("StartLabSession receive: %v", err)
	}
	lab := h.module.labs.session(session.ID)
	if lab == nil {
		t.Fatal("receive lab session was not retained")
	}
	receiveRTP, receiveRTCP := lab.mediaAddresses()
	peerAddr := lab.peerAddress()
	active := waitForGBLabSnapshot(t, h.module, session.ID, func(snapshot LabSessionSnapshot) bool {
		return snapshot.State == LabSessionStateActive &&
			snapshot.RTPPacketsRecv > 0 && snapshot.RTCPPacketsRecv > 0 && snapshot.PSFramesRecv > 0
	})
	if active.Direction != LabDirectionOutbound {
		t.Fatalf("receive snapshot = %+v, want outbound", active)
	}
	waitForGBLab(t, func() bool { return lab.catalogSent.Load() })

	if err := h.module.StopLabSession(session.ID); err != nil {
		t.Fatalf("StopLabSession receive: %v", err)
	}
	if err := h.module.StopLabSession(session.ID); err != nil {
		t.Fatalf("second StopLabSession receive: %v", err)
	}
	waitForGBLab(t, func() bool {
		return len(h.module.sessions.All()) == 0 && h.module.registry.Get(request.DeviceID) == nil
	})
	assertGBLabPortFree(t, receiveRTP)
	assertGBLabPortFree(t, receiveRTCP)
	assertGBLabPortFree(t, peerAddr)
	if stream.Publisher() == nil {
		t.Fatal("receive teardown removed the pre-existing source publisher")
	}
}

func TestGBLabRejectsDuplicateIdentity(t *testing.T) {
	h := newRealGBLabHarness(t)
	request := LabSessionRequest{
		Mode:      LabModePublish,
		DeviceID:  "34020000001320000031",
		ChannelID: "34020000001320000032",
		StreamKey: "gb28181/duplicate",
	}
	if _, err := h.module.StartLabSession(context.Background(), request); err != nil {
		t.Fatalf("first StartLabSession: %v", err)
	}
	if _, err := h.module.StartLabSession(context.Background(), request); !errors.Is(err, ErrLabDuplicateIdentity) {
		t.Fatalf("duplicate StartLabSession error = %v, want ErrLabDuplicateIdentity", err)
	}
}

type realGBLabHarness struct {
	module *Module
	hub    *core.StreamHub
}

func newRealGBLabHarness(t *testing.T) realGBLabHarness {
	t.Helper()
	cfg := &config.Config{
		SIP: config.SIPConfig{
			Enabled:   true,
			Listen:    freeGBLabAddress(t),
			Transport: []string{"udp"},
			ServerID:  "liveforge-gb-lab",
			Domain:    "lab.local",
		},
		GB28181: config.GB28181Config{
			Enabled:      true,
			StreamPrefix: "gb28181",
			RTPPortRange: []int{freeGBLabPort(t), 0},
			Keepalive:    config.KeepaliveConfig{Timeout: time.Minute},
		},
		Stream: config.StreamConfig{GOPCache: true, GOPCacheNum: 1, RingBufferSize: 256},
	}
	cfg.GB28181.RTPPortRange[1] = cfg.GB28181.RTPPortRange[0] + 100
	server := core.NewServer(cfg)
	sipModule := sipmod.NewModule()
	gbModule := NewModule(sipModule.Service())
	server.RegisterModule(sipModule)
	server.RegisterModule(gbModule)
	if err := server.Init(); err != nil {
		t.Fatalf("Init real GB lab harness: %v", err)
	}
	t.Cleanup(server.Shutdown)
	time.Sleep(25 * time.Millisecond)
	return realGBLabHarness{module: gbModule, hub: server.StreamHub()}
}

func freeGBLabAddress(t *testing.T) string {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("reserve SIP address: %v", err)
	}
	defer conn.Close()
	return conn.LocalAddr().String()
}

func freeGBLabPort(t *testing.T) int {
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

func deterministicGBLabTestFrame(timestamp int64) *avframe.AVFrame {
	startCode := []byte{0, 0, 0, 1}
	payload := append([]byte{}, startCode...)
	payload = append(payload, []byte{0x67, 0x42, 0x00, 0x1e, 0xab, 0x40, 0x50}...)
	payload = append(payload, startCode...)
	payload = append(payload, []byte{0x68, 0xce, 0x38, 0x80}...)
	payload = append(payload, startCode...)
	payload = append(payload, 0x65, 1, 2, 3, 4, 5, 6, 7, 8)
	return avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, timestamp, timestamp, payload)
}

func waitForGBLabSnapshot(t *testing.T, module *Module, id string, predicate func(LabSessionSnapshot) bool) LabSessionSnapshot {
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
	t.Fatalf("GB lab session %q did not reach expected state; snapshots=%+v", id, module.ListLabSessions())
	return LabSessionSnapshot{}
}

func waitForGBLab(t *testing.T, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("GB lab condition was not reached")
}

func (m *labManager) session(id string) *gbLabSession {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sessions[id]
}

func (s *gbLabSession) mediaAddresses() (string, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.rtpConn.LocalAddr().String(), s.rtcpConn.LocalAddr().String()
}

func (s *gbLabSession) peerAddress() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.peerConn == nil {
		return ""
	}
	return s.peerConn.LocalAddr().String()
}

func assertGBLabPortFree(t *testing.T, address string) {
	t.Helper()
	if address == "" {
		t.Fatal("missing lab address")
	}
	udpAddress, err := net.ResolveUDPAddr("udp4", address)
	if err != nil {
		t.Fatalf("resolve released lab address %q: %v", address, err)
	}
	conn, err := net.ListenUDP("udp4", udpAddress)
	if err != nil {
		t.Fatalf("lab address %q was not released: %v", address, err)
	}
	_ = conn.Close()
}
