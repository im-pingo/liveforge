package gb28181

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	sipmod "github.com/im-pingo/liveforge/module/sip"
	sipgateway "github.com/im-pingo/liveforge/module/sipgateway"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/codec/h264"
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
	waitForGBLab(t, func() bool {
		stats := stream.Stats()
		gop := stream.GOPCacheDetail()
		return stats.VideoFrames >= 10 && stats.AudioFrames >= 10 &&
			gop.VideoFrames >= 2 && gop.AudioFrames >= 2 &&
			gop.DurationMs >= 40 && gop.DurationMs <= 1200
	})
	mediaInfo := stream.Publisher().MediaInfo()
	if mediaInfo == nil || mediaInfo.VideoCodec != avframe.CodecH264 || mediaInfo.AudioCodec != avframe.CodecG711A || mediaInfo.SampleRate != 8000 || mediaInfo.Channels != 1 {
		t.Fatalf("publish media info = %+v, want H264/G711A at 8000 Hz mono", mediaInfo)
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

func TestGBLabPublishRefreshesRegistrationKeepalive(t *testing.T) {
	h := newRealGBLabHarness(t)
	h.module.registry.keepaliveTimeout = 300 * time.Millisecond
	request := LabSessionRequest{
		Mode:      LabModePublish,
		DeviceID:  "34020000001320000061",
		ChannelID: "34020000001320000062",
		StreamKey: "gb28181/keepalive",
	}

	session, err := h.module.StartLabSession(context.Background(), request)
	if err != nil {
		t.Fatalf("StartLabSession publish: %v", err)
	}
	t.Cleanup(func() { _ = h.module.StopLabSession(session.ID) })
	waitForGBLabSnapshot(t, h.module, session.ID, func(snapshot LabSessionSnapshot) bool {
		return snapshot.State == LabSessionStateActive && snapshot.RTPPacketsSent > 0
	})

	firstKeepalive := gbLabLastKeepalive(h.module.registry, request.DeviceID)
	waitForGBLab(t, func() bool {
		return gbLabLastKeepalive(h.module.registry, request.DeviceID).After(firstKeepalive)
	})

	time.Sleep(350 * time.Millisecond)
	h.module.registry.checkKeepalives(nil)
	device := h.module.registry.Get(request.DeviceID)
	if device == nil || device.Status != DeviceStatusOnline {
		t.Fatalf("device after keepalive timeout = %+v, want online", device)
	}
}

func TestCombinedSIPAndGB28181RoutesAudioInviteToSIPGateway(t *testing.T) {
	h := newCombinedSIPAndGBLabHarness(t)
	request := sipgateway.LabSessionRequest{
		Mode:      sipgateway.LabModePublish,
		DeviceID:  "d1",
		StreamKey: "s1",
		Codec:     "PCMA",
	}
	session, err := h.gateway.StartLabSession(context.Background(), request)
	if err != nil {
		t.Fatalf("StartLabSession publish: %v", err)
	}
	t.Cleanup(func() { _ = h.gateway.StopLabSession(session.ID) })

	var active sipgateway.LabSessionSnapshot
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, snapshot := range h.gateway.ListLabSessions() {
			if snapshot.ID == session.ID && snapshot.State == sipgateway.LabSessionStateActive && snapshot.RTPPacketsSent > 0 {
				active = snapshot
				break
			}
		}
		if active.State == sipgateway.LabSessionStateActive {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if active.State != sipgateway.LabSessionStateActive {
		t.Fatalf("SIP lab snapshot = %+v, want active audio publisher; all=%+v", active, h.gateway.ListLabSessions())
	}

	stream, ok := h.server.StreamHub().Find(request.StreamKey)
	if !ok || stream.Publisher() == nil {
		t.Fatalf("stream %q = (%v, %v), want active SIP publisher", request.StreamKey, stream, ok)
	}
	if !strings.HasPrefix(stream.Publisher().ID(), "sip-") {
		t.Fatalf("stream %q publisher ID = %q, want SIP gateway publisher", request.StreamKey, stream.Publisher().ID())
	}
}

func TestCombinedSIPAndGB28181RoutesVideoInviteToGB28181(t *testing.T) {
	h := newCombinedSIPAndGBLabHarness(t)
	request := LabSessionRequest{
		Mode:      LabModePublish,
		DeviceID:  "34020000001320000051",
		ChannelID: "34020000001320000052",
		StreamKey: "gb28181/combined-video",
	}
	session, err := h.gb.StartLabSession(context.Background(), request)
	if err != nil {
		t.Fatalf("StartLabSession GB28181 publish: %v", err)
	}
	t.Cleanup(func() { _ = h.gb.StopLabSession(session.ID) })

	active := waitForGBLabSnapshot(t, h.gb, session.ID, func(snapshot LabSessionSnapshot) bool {
		return snapshot.State == LabSessionStateActive && snapshot.RTPPacketsSent > 0 && snapshot.PSFramesSent > 0
	})
	if active.Direction != LabDirectionInbound {
		t.Fatalf("GB28181 lab snapshot = %+v, want inbound video publisher", active)
	}
	stream, ok := h.server.StreamHub().Find(request.StreamKey)
	if !ok || stream.Publisher() == nil {
		t.Fatalf("stream %q = (%v, %v), want active GB28181 publisher", request.StreamKey, stream, ok)
	}
	if !strings.HasPrefix(stream.Publisher().ID(), "gb28181-live-") {
		t.Fatalf("stream %q publisher ID = %q, want GB28181 publisher", request.StreamKey, stream.Publisher().ID())
	}
	if calls := h.gateway.ListCalls(); len(calls) != 0 {
		t.Fatalf("SIP Gateway calls after GB28181 video INVITE = %+v, want none", calls)
	}
}

type combinedSIPAndGBLabHarness struct {
	server  *core.Server
	gb      *Module
	gateway *sipgateway.Module
}

func newCombinedSIPAndGBLabHarness(t *testing.T) combinedSIPAndGBLabHarness {
	t.Helper()
	sipAddr := freeGBLabAddress(t)
	gbPort := freeGBLabPort(t)
	gatewayPort := freeGBLabPortExcluding(t, gbPort)

	cfg := &config.Config{
		SIP: config.SIPConfig{
			Enabled:   true,
			Listen:    sipAddr,
			Transport: []string{"udp"},
			ServerID:  "liveforge-combined-lab",
			Domain:    "lab.local",
			Gateway: config.SIPGatewayConfig{
				Enabled:      true,
				StreamPrefix: "sip",
				RTPPortRange: []int{gatewayPort, gatewayPort + 100},
				Codecs:       []string{"PCMA", "PCMU"},
				MaxCalls:     8,
			},
		},
		GB28181: config.GB28181Config{
			Enabled:      true,
			StreamPrefix: "gb28181",
			RTPPortRange: []int{gbPort, gbPort + 100},
			Keepalive:    config.KeepaliveConfig{Timeout: time.Minute},
		},
		Stream: config.StreamConfig{GOPCache: true, GOPCacheNum: 1, RingBufferSize: 256},
	}
	server := core.NewServer(cfg)
	sipModule := sipmod.NewModule()
	gbModule := NewModule(sipModule.Service())
	gatewayModule := sipgateway.NewModule(sipModule.Service())
	server.RegisterModule(sipModule)
	server.RegisterModule(gbModule)
	server.RegisterModule(gatewayModule)
	if err := server.Init(); err != nil {
		t.Fatalf("Init combined SIP and GB28181 lab harness: %v", err)
	}
	t.Cleanup(server.Shutdown)
	time.Sleep(25 * time.Millisecond)
	return combinedSIPAndGBLabHarness{server: server, gb: gbModule, gateway: gatewayModule}
}

func TestGB28181InviteRecognitionRejectsSIPAudioAndOtherVideo(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "GB28181 PS video",
			body: "v=0\r\nm=video 30000 RTP/AVP 96\r\na=rtpmap:96 PS/90000\r\n",
			want: true,
		},
		{
			name: "legacy video without rtpmap",
			body: "v=0\r\nm=video 30000 RTP/AVP 96\r\n",
			want: true,
		},
		{
			name: "SIP PCMA audio",
			body: "v=0\r\nm=audio 30000 RTP/AVP 8\r\na=rtpmap:8 PCMA/8000\r\n",
			want: false,
		},
		{
			name: "H264 video",
			body: "v=0\r\nm=video 30000 RTP/AVP 96\r\na=rtpmap:96 H264/90000\r\n",
			want: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := newGBRequest(sip.INVITE, "device", "channel")
			req.SetBody([]byte(test.body))
			if got := isGB28181VideoInvite(req); got != test.want {
				t.Fatalf("isGB28181VideoInvite = %v, want %v", got, test.want)
			}
		})
	}
}

func TestDeterministicGBLabFrameContainsDecodableH264ParameterSets(t *testing.T) {
	first := deterministicGBLabFrame(0)
	nalus := h264.ExtractNALUs(first.Payload)
	if len(nalus) < 3 {
		t.Fatalf("deterministic frame NAL units = %d, want SPS/PPS/IDR", len(nalus))
	}
	var sps []byte
	var hasPPS, hasIDR bool
	for _, nalu := range nalus {
		switch nalu[0] & 0x1f {
		case h264.NALTypeSPS:
			sps = nalu
		case h264.NALTypePPS:
			hasPPS = true
		case h264.NALTypeIDR:
			hasIDR = true
		}
	}
	if len(sps) == 0 || !hasPPS || !hasIDR {
		t.Fatalf("deterministic frame NAL units missing SPS/PPS/IDR: %v", nalus)
	}
	info, err := h264.ParseSPS(sps)
	if err != nil {
		t.Fatalf("ParseSPS: %v", err)
	}
	if info.Width < 160 || info.Height < 90 {
		t.Fatalf("SPS dimensions = %dx%d, want at least 160x90", info.Width, info.Height)
	}

	next := deterministicGBLabFrame(40)
	if next.FrameType != avframe.FrameTypeInterframe {
		t.Fatalf("second deterministic frame type = %v, want interframe", next.FrameType)
	}
	if bytes.Equal(first.Payload, next.Payload) {
		t.Fatal("consecutive deterministic video frames are identical")
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
	if port+100 > 65535 {
		port = 60000
	}
	return port
}

func freeGBLabPortExcluding(t *testing.T, other int) int {
	t.Helper()
	for attempt := 0; attempt < 100; attempt++ {
		port := freeGBLabPort(t)
		if port > other+100 || other > port+100 {
			return port
		}
	}
	t.Fatalf("could not find a free GB lab port range outside %d", other)
	return 0
}

func deterministicGBLabTestFrame(timestamp int64) *avframe.AVFrame {
	return deterministicGBLabFrame(timestamp)
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

func gbLabLastKeepalive(registry *DeviceRegistry, deviceID string) time.Time {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	if device := registry.devices[deviceID]; device != nil {
		return device.LastKeepalive
	}
	return time.Time{}
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
