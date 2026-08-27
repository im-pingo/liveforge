package gb28181

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/internal/labmedia"
	sipmod "github.com/im-pingo/liveforge/module/sip"
	sipgateway "github.com/im-pingo/liveforge/module/sipgateway"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/codec/h264"
	"github.com/im-pingo/liveforge/pkg/muxer/ps"
	pionrtp "github.com/pion/rtp/v2"
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

func TestLabManagerRejectsAmbiguousStreamKeySegments(t *testing.T) {
	for _, streamKey := range []string{"/tenant/cam", "tenant/cam/", "tenant//cam", "tenant/./cam", "tenant/../cam", ".", ".."} {
		t.Run(streamKey, func(t *testing.T) {
			request := validGBLabRequest()
			request.StreamKey = streamKey
			if err := validateGBLabRequest(request); !errors.Is(err, ErrLabInvalidRequest) {
				t.Fatalf("validateGBLabRequest(%q) error = %v, want ErrLabInvalidRequest", streamKey, err)
			}
		})
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

func TestGBLabPublishUsesServerInviteAndRegisteredDeviceContact(t *testing.T) {
	h := newRealGBLabHarness(t)
	request := LabSessionRequest{
		Mode:      LabModePublish,
		DeviceID:  "34020000001320000071",
		ChannelID: "34020000001320000072",
		StreamKey: "gb28181/server-invite",
	}

	session, err := h.module.StartLabSession(context.Background(), request)
	if err != nil {
		t.Fatalf("StartLabSession publish: %v", err)
	}
	t.Cleanup(func() { _ = h.module.StopLabSession(session.ID) })
	lab := h.module.labs.session(session.ID)
	if lab == nil {
		t.Fatal("publish lab session was not retained")
	}
	waitForGBLabSnapshot(t, h.module, session.ID, func(snapshot LabSessionSnapshot) bool {
		return snapshot.State == LabSessionStateActive && snapshot.RTPPacketsSent > 0 && snapshot.RTCPPacketsRecv > 0
	})

	device := h.module.registry.Get(request.DeviceID)
	if device == nil {
		t.Fatal("registered fake device was not found")
	}
	peerAddress := lab.peerAddress()
	if peerAddress == "" || device.RemoteAddr != peerAddress {
		t.Fatalf("registered device contact = %q, fake listener = %q; want server INVITE routed to fake listener", device.RemoteAddr, peerAddress)
	}

	sessions := h.module.sessions.GetByChannel(request.ChannelID)
	if len(sessions) != 1 {
		t.Fatalf("module media sessions = %d, want one server-initiated session", len(sessions))
	}
	serverSession := sessions[0].Snapshot()
	if serverSession.Direction != SessionDirectionOutbound {
		t.Fatalf("module media direction = %v, want server-initiated outbound", serverSession.Direction)
	}
	if serverSession.Receiver == nil {
		t.Fatal("server-initiated publish session has no LiveForge RTP receiver")
	}

	rtcpAddr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: serverSession.LocalPort + 1}
	probe, bindErr := net.ListenUDP("udp4", rtcpAddr)
	if bindErr == nil {
		_ = probe.Close()
		t.Fatalf("LiveForge RTCP port %d was not bound by the real receiver", rtcpAddr.Port)
	}
	if err := h.module.StopLabSession(session.ID); err != nil {
		t.Fatalf("StopLabSession publish: %v", err)
	}
	waitForGBLab(t, lab.byeReceived.Load)
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
	server          *core.Server
	gb              *Module
	gateway         *sipgateway.Module
	gbRTPRange      []int
	gatewayRTPRange []int
}

func TestGBLabHarnessRTPRangesAreMinimalVerifiedAndDisjoint(t *testing.T) {
	real := newRealGBLabHarness(t)
	_, realSIPPort, err := sipAddress(real.module.sipService.LocalAddr())
	if err != nil {
		t.Fatalf("parse real GB Lab SIP address: %v", err)
	}
	assertGBLabHarnessRTPRange(t, real.rtpPortRange, 2, realSIPPort)
	assertGBLabHarnessRangesFree(t, real.rtpPortRange)

	combined := newCombinedSIPAndGBLabHarness(t)
	_, combinedSIPPort, err := sipAddress(combined.gb.sipService.LocalAddr())
	if err != nil {
		t.Fatalf("parse combined Lab SIP address: %v", err)
	}
	assertGBLabHarnessRTPRange(t, combined.gbRTPRange, 1, combinedSIPPort)
	assertGBLabHarnessRTPRange(t, combined.gatewayRTPRange, 2, combinedSIPPort)
	if combined.gbRTPRange[0] <= combined.gatewayRTPRange[1] && combined.gatewayRTPRange[0] <= combined.gbRTPRange[1] {
		t.Fatalf("combined GB range %v overlaps SIP Gateway range %v", combined.gbRTPRange, combined.gatewayRTPRange)
	}
	assertGBLabHarnessRangesFree(t, combined.gbRTPRange, combined.gatewayRTPRange)
}

func assertGBLabHarnessRTPRange(t *testing.T, rtpRange []int, pairCount, excludedPort int) {
	t.Helper()
	wantPortCount := pairCount * 2
	if len(rtpRange) != 2 || rtpRange[0]%2 != 0 || rtpRange[1] != rtpRange[0]+wantPortCount-1 {
		t.Fatalf("RTP range = %v, want exactly %d contiguous even-aligned RTP/RTCP pairs", rtpRange, pairCount)
	}
	if excludedPort >= rtpRange[0] && excludedPort <= rtpRange[1] {
		t.Fatalf("SIP control port %d overlaps RTP range %v", excludedPort, rtpRange)
	}
}

func assertGBLabHarnessRangesFree(t *testing.T, ranges ...[]int) {
	t.Helper()
	reservations := make([]*net.UDPConn, 0)
	defer func() {
		for _, conn := range reservations {
			_ = conn.Close()
		}
	}()
	for _, rtpRange := range ranges {
		for port := rtpRange[0]; port <= rtpRange[1]; port++ {
			conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: port})
			if err != nil {
				t.Fatalf("RTP range port %d is not free: %v", port, err)
			}
			reservations = append(reservations, conn)
		}
	}
}

func newCombinedSIPAndGBLabHarness(t *testing.T) combinedSIPAndGBLabHarness {
	t.Helper()
	sipAddr := freeGBLabAddress(t)
	_, sipPort, err := sipAddress(sipAddr)
	if err != nil {
		t.Fatalf("parse combined Lab SIP address: %v", err)
	}
	gbRTPRange := freeGBLabRTPPortRange(t, 1, []int{sipPort, sipPort})
	gatewayRTPRange := freeGBLabRTPPortRange(t, 2, []int{sipPort, sipPort}, gbRTPRange)

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
				RTPPortRange: gatewayRTPRange,
				Codecs:       []string{"PCMA", "PCMU"},
				MaxCalls:     8,
			},
		},
		GB28181: config.GB28181Config{
			Enabled:      true,
			StreamPrefix: "gb28181",
			RTPPortRange: gbRTPRange,
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
	return combinedSIPAndGBLabHarness{
		server:          server,
		gb:              gbModule,
		gateway:         gatewayModule,
		gbRTPRange:      append([]int(nil), gbRTPRange...),
		gatewayRTPRange: append([]int(nil), gatewayRTPRange...),
	}
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

func TestGBLabRTPStartsFromOneOffsetAndRemainsMonotonic(t *testing.T) {
	remoteRTP, remoteRTCP, err := listenGBLabUDPPair()
	if err != nil {
		t.Fatalf("listen remote media pair: %v", err)
	}
	defer remoteRTP.Close()
	defer remoteRTCP.Close()
	senderRTP, senderRTCP, err := listenGBLabUDPPair()
	if err != nil {
		t.Fatalf("listen sender media pair: %v", err)
	}
	defer senderRTP.Close()
	defer senderRTCP.Close()
	session := newGBLabSession("timestamp-test", "timestamp-test", validGBLabRequest(), nil)
	defer session.cancel()
	session.rtpConn = senderRTP
	session.rtcpConn = senderRTCP
	muxer := ps.NewMuxer()
	remoteRTPAddr := remoteRTP.LocalAddr().(*net.UDPAddr)
	remoteRTCPAddr := remoteRTCP.LocalAddr().(*net.UDPAddr)

	if err := session.sendFrame(remoteRTPAddr, remoteRTCPAddr, muxer, deterministicGBLabFrame(0)); err != nil {
		t.Fatalf("send first video frame: %v", err)
	}
	videoTimestamp, _, _ := readGBRTPFrame(t, remoteRTP)
	if err := session.sendFrame(remoteRTPAddr, remoteRTCPAddr, muxer, labmedia.G711Frame(avframe.CodecG711A, 0)); err != nil {
		t.Fatalf("send first audio frame: %v", err)
	}
	audioTimestamp, _, _ := readGBRTPFrame(t, remoteRTP)
	if err := session.sendFrame(remoteRTPAddr, remoteRTCPAddr, muxer, labmedia.G711Frame(avframe.CodecG711A, 20)); err != nil {
		t.Fatalf("send second audio frame: %v", err)
	}
	nextTimestamp, _, _ := readGBRTPFrame(t, remoteRTP)

	if videoTimestamp != audioTimestamp {
		t.Fatalf("first video/audio RTP timestamps = %d/%d, want one shared offset", videoTimestamp, audioTimestamp)
	}
	if delta := nextTimestamp - audioTimestamp; delta != 20*90 {
		t.Fatalf("next RTP timestamp delta = %d, want %d", delta, 20*90)
	}
}

func readGBRTPFrame(t *testing.T, conn *net.UDPConn) (timestamp uint32, packets uint32, payloadOctets uint32) {
	t.Helper()
	buf := make([]byte, 2048)
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set RTP read deadline: %v", err)
	}
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			t.Fatalf("read RTP packet: %v", err)
		}
		var packet pionrtp.Packet
		if err := packet.Unmarshal(buf[:n]); err != nil {
			t.Fatalf("unmarshal RTP packet: %v", err)
		}
		if packets == 0 {
			timestamp = packet.Timestamp
		} else if packet.Timestamp != timestamp {
			t.Fatalf("RTP frame changed timestamp from %d to %d", timestamp, packet.Timestamp)
		}
		packets++
		payloadOctets += uint32(len(packet.Payload))
		if packet.Marker {
			return timestamp, packets, payloadOctets
		}
	}
}

func TestGBLabReceiveAcceptsRealLivePlayAndCountsMedia(t *testing.T) {
	h := newRealGBLabHarness(t)
	stream, err := h.hub.GetOrCreate("gb28181/source")
	if err != nil {
		t.Fatalf("GetOrCreate source: %v", err)
	}
	publisher := &gbLabSourcePublisher{id: "gb28181-source", info: avframe.MediaInfo{
		VideoCodec: avframe.CodecH264,
		AudioCodec: avframe.CodecG711A,
		SampleRate: 8000,
		Channels:   1,
	}}
	if err := stream.SetPublisher(publisher); err != nil {
		t.Fatalf("SetPublisher source: %v", err)
	}
	for timestamp := int64(0); timestamp < 200; timestamp += labmedia.AudioFrameDurationMs {
		if timestamp%labmedia.VideoFrameDurationMs == 0 {
			stream.WriteFrame(deterministicGBLabTestFrame(timestamp))
		}
		stream.WriteFrame(labmedia.G711Frame(avframe.CodecG711A, timestamp))
	}
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
	encoded, err := json.Marshal(active)
	if err != nil {
		t.Fatalf("marshal receive snapshot: %v", err)
	}
	var counters map[string]any
	if err := json.Unmarshal(encoded, &counters); err != nil {
		t.Fatalf("decode receive counters: %v", err)
	}
	if counters["audio_frames_received"] == nil || counters["audio_frames_received"].(float64) <= 0 {
		t.Fatalf("receive counters = %s, want audio frames received by fake device", encoded)
	}
	if counters["video_frames_received"] == nil || counters["video_frames_received"].(float64) <= 0 {
		t.Fatalf("receive counters = %s, want video frames received by fake device", encoded)
	}
	serverSessions := h.module.sessions.GetByChannel(request.ChannelID)
	if len(serverSessions) != 1 || serverSessions[0].Snapshot().Direction != SessionDirectionOutbound || serverSessions[0].Snapshot().Publisher != nil {
		t.Fatalf("module egress sessions = %+v, want one outbound non-publisher media session", serverSessions)
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

func TestGBLabReceiveRejectsIncompatibleSourceBeforeActivation(t *testing.T) {
	h := newRealGBLabHarness(t)
	stream, err := h.hub.GetOrCreate("gb28181/incompatible-source")
	if err != nil {
		t.Fatalf("GetOrCreate source: %v", err)
	}
	if err := stream.SetPublisher(&gbLabSourcePublisher{id: "incompatible", info: avframe.MediaInfo{
		VideoCodec: avframe.CodecH264,
		AudioCodec: avframe.CodecAAC,
	}}); err != nil {
		t.Fatalf("SetPublisher source: %v", err)
	}

	_, err = h.module.StartLabSession(context.Background(), LabSessionRequest{
		Mode:      LabModeReceive,
		DeviceID:  "34020000001320000081",
		ChannelID: "34020000001320000082",
		StreamKey: stream.Key(),
	})
	if !errors.Is(err, ErrLabInvalidRequest) {
		t.Fatalf("StartLabSession incompatible source error = %v, want ErrLabInvalidRequest", err)
	}
	if got := len(h.module.sessions.All()); got != 0 {
		t.Fatalf("module sessions after incompatible source = %d, want 0", got)
	}
}

func TestGBLabReceiveRejectsSubscriberAdmissionBeforeActivation(t *testing.T) {
	h := newRealGBLabHarnessWithConfig(t, func(cfg *config.Config) {
		cfg.Limits.MaxSubscribersPerStream = 1
	})
	stream, err := h.hub.GetOrCreate("gb28181/subscriber-limit-source")
	if err != nil {
		t.Fatalf("GetOrCreate source: %v", err)
	}
	if err := stream.SetPublisher(&gbLabSourcePublisher{id: "subscriber-limit-source", info: avframe.MediaInfo{
		VideoCodec: avframe.CodecH264,
		AudioCodec: avframe.CodecG711A,
		SampleRate: 8000,
		Channels:   1,
	}}); err != nil {
		t.Fatalf("SetPublisher source: %v", err)
	}
	if err := stream.AddSubscriber("occupied"); err != nil {
		t.Fatalf("occupy subscriber capacity: %v", err)
	}
	t.Cleanup(func() { stream.RemoveSubscriber("occupied") })

	request := LabSessionRequest{
		Mode:      LabModeReceive,
		DeviceID:  "34020000001320000091",
		ChannelID: "34020000001320000092",
		StreamKey: stream.Key(),
	}
	failed, err := h.module.StartLabSession(context.Background(), request)
	if err == nil || !strings.Contains(err.Error(), "max subscribers per stream") {
		t.Fatalf("StartLabSession subscriber rejection = (%+v, %v), want synchronous limit error", failed, err)
	}
	if failed.State == LabSessionStateActive {
		t.Fatalf("subscriber-rejected Lab state = %q, must never become active", failed.State)
	}
	for _, snapshot := range h.module.ListLabSessions() {
		if snapshot.ID == failed.ID && snapshot.State == LabSessionStateActive {
			t.Fatalf("subscriber-rejected Lab remained active: %+v", snapshot)
		}
	}
	if got := len(h.module.sessions.All()); got != 0 {
		t.Fatalf("subscriber rejection left %d module sessions", got)
	}
	lab := h.module.labs.session(failed.ID)
	if lab == nil {
		t.Fatal("subscriber-rejected Lab terminal record was not retained")
	}
	lab.mu.RLock()
	moduleSession := lab.moduleSession
	lab.mu.RUnlock()
	if moduleSession != nil {
		t.Fatalf("subscriber rejection created module dialog/session: %+v", moduleSession.Snapshot())
	}
	if h.module.registry.Get(request.DeviceID) != nil {
		t.Fatal("subscriber rejection left fake device registered")
	}
	assertGBLabPortFree(t, lab.peerAddress())
	fakeRTP, fakeRTCP := lab.mediaAddresses()
	assertGBLabPortFree(t, fakeRTP)
	assertGBLabPortFree(t, fakeRTCP)
	assertGBLabModulePortPairFree(t, h)
	if got := stream.Subscribers()["gb28181"]; got != 0 {
		t.Fatalf("subscriber rejection left %d GB28181 subscribers", got)
	}
}

func TestGBLabReceiveWorkerFailureTransitionsToFailedAndCleansUp(t *testing.T) {
	h := newRealGBLabHarness(t)
	stream, err := h.hub.GetOrCreate("gb28181/worker-failure-source")
	if err != nil {
		t.Fatalf("GetOrCreate source: %v", err)
	}
	if err := stream.SetPublisher(&gbLabSourcePublisher{id: "worker-failure-source", info: avframe.MediaInfo{
		VideoCodec: avframe.CodecH264,
		AudioCodec: avframe.CodecG711A,
		SampleRate: 8000,
		Channels:   1,
	}}); err != nil {
		t.Fatalf("SetPublisher source: %v", err)
	}

	request := LabSessionRequest{
		Mode:      LabModeReceive,
		DeviceID:  "34020000001320000101",
		ChannelID: "34020000001320000102",
		StreamKey: stream.Key(),
	}
	active, err := h.module.StartLabSession(context.Background(), request)
	if err != nil {
		t.Fatalf("StartLabSession receive: %v", err)
	}
	lab := h.module.labs.session(active.ID)
	if lab == nil {
		t.Fatal("active receive Lab was not retained")
	}
	lab.mu.RLock()
	moduleSession := lab.moduleSession
	lab.mu.RUnlock()
	if moduleSession == nil {
		t.Fatal("active receive Lab has no module-owned sender session")
	}
	moduleSnapshot := moduleSession.Snapshot()
	if moduleSnapshot.Sender == nil {
		t.Fatal("active receive Lab has no outbound sender")
	}
	moduleRTP := moduleSnapshot.Sender.rtpConn.LocalAddr().String()
	moduleRTCP := moduleSnapshot.Sender.rtcpConn.LocalAddr().String()
	fakeRTP, fakeRTCP := lab.mediaAddresses()
	peerAddress := lab.peerAddress()
	if err := moduleSnapshot.Sender.rtpConn.Close(); err != nil {
		t.Fatalf("close outbound RTP socket: %v", err)
	}
	stream.WriteFrame(deterministicGBLabTestFrame(0))

	failed := waitForGBLabSnapshot(t, h.module, active.ID, func(snapshot LabSessionSnapshot) bool {
		return snapshot.State == LabSessionStateFailed && strings.Contains(snapshot.LastError, "GB28181 outbound media send")
	})
	if failed.LastError == "" || strings.ContainsAny(failed.LastError, "\r\n") || len([]rune(failed.LastError)) > gbLabTerminalErrorLimit {
		t.Fatalf("worker failure diagnostic was not bounded/redacted: %q", failed.LastError)
	}
	if got := len(h.module.sessions.All()); got != 0 {
		t.Fatalf("worker failure left %d module sessions", got)
	}
	if h.module.sessions.Get(moduleSnapshot.ID) != nil {
		t.Fatalf("worker failure left module session %q registered", moduleSnapshot.ID)
	}
	waitForGBLab(t, lab.byeReceived.Load)
	if h.module.registry.Get(request.DeviceID) != nil {
		t.Fatal("worker failure left fake device registered")
	}
	if got := stream.Subscribers()["gb28181"]; got != 0 {
		t.Fatalf("worker failure left %d GB28181 subscribers", got)
	}
	for _, address := range []string{moduleRTP, moduleRTCP, fakeRTP, fakeRTCP, peerAddress} {
		assertGBLabPortFree(t, address)
	}
	assertGBLabModulePortPairFree(t, h)
}

func TestGBLabPublishMediaWorkerFailureTransitionsToFailedAndCleansUp(t *testing.T) {
	h := newRealGBLabHarness(t)
	request := LabSessionRequest{
		Mode:      LabModePublish,
		DeviceID:  "34020000001320000111",
		ChannelID: "34020000001320000112",
		StreamKey: "gb28181/publish-worker-failure",
	}
	active, err := h.module.StartLabSession(context.Background(), request)
	if err != nil {
		t.Fatalf("StartLabSession publish: %v", err)
	}
	t.Cleanup(func() { _ = h.module.StopLabSession(active.ID) })
	lab := h.module.labs.session(active.ID)
	if lab == nil {
		t.Fatal("active publish Lab was not retained")
	}
	waitForGBLabSnapshot(t, h.module, active.ID, func(snapshot LabSessionSnapshot) bool {
		return snapshot.State == LabSessionStateActive && snapshot.RTPPacketsSent > 0
	})
	lab.mu.RLock()
	moduleSession := lab.moduleSession
	fakeRTP, fakeRTCP := lab.rtpConn.LocalAddr().String(), lab.rtcpConn.LocalAddr().String()
	peerAddress := lab.peerConn.LocalAddr().String()
	lab.mu.RUnlock()
	if moduleSession == nil {
		t.Fatal("publish Lab has no server receiver session")
	}
	modulePort := moduleSession.Snapshot().LocalPort
	if err := lab.rtpConn.Close(); err != nil {
		t.Fatalf("close fake-device RTP socket: %v", err)
	}

	failed := waitForGBLabSnapshot(t, h.module, active.ID, func(snapshot LabSessionSnapshot) bool {
		return snapshot.State == LabSessionStateFailed && strings.Contains(snapshot.LastError, "publish media")
	})
	if failed.LastError == "" {
		t.Fatal("publish worker failure has no terminal error")
	}
	assertGBLabFailureCleanup(t, h, lab, request.DeviceID, moduleSession, []string{
		fakeRTP, fakeRTCP, peerAddress,
		net.JoinHostPort("127.0.0.1", fmt.Sprint(modulePort)),
		net.JoinHostPort("127.0.0.1", fmt.Sprint(modulePort+1)),
	})
}

func TestGBLabPublishReceiverFailureTransitionsToFailedAndCleansUp(t *testing.T) {
	h := newRealGBLabHarness(t)
	request := LabSessionRequest{
		Mode:      LabModePublish,
		DeviceID:  "34020000001320000121",
		ChannelID: "34020000001320000122",
		StreamKey: "gb28181/publish-receiver-failure",
	}
	active, err := h.module.StartLabSession(context.Background(), request)
	if err != nil {
		t.Fatalf("StartLabSession publish: %v", err)
	}
	t.Cleanup(func() { _ = h.module.StopLabSession(active.ID) })
	lab := h.module.labs.session(active.ID)
	if lab == nil {
		t.Fatal("active publish Lab was not retained")
	}
	lab.mu.RLock()
	moduleSession := lab.moduleSession
	fakeRTP, fakeRTCP := lab.rtpConn.LocalAddr().String(), lab.rtcpConn.LocalAddr().String()
	peerAddress := lab.peerConn.LocalAddr().String()
	lab.mu.RUnlock()
	if moduleSession == nil || moduleSession.Snapshot().Receiver == nil {
		t.Fatal("publish Lab has no real server RTP receiver")
	}
	moduleSnapshot := moduleSession.Snapshot()
	modulePort := moduleSnapshot.LocalPort
	if err := moduleSnapshot.Receiver.conn.Close(); err != nil {
		t.Fatalf("close server RTP receiver socket: %v", err)
	}

	failed := waitForGBLabSnapshot(t, h.module, active.ID, func(snapshot LabSessionSnapshot) bool {
		return snapshot.State == LabSessionStateFailed && strings.Contains(snapshot.LastError, "receiver")
	})
	if failed.LastError == "" {
		t.Fatal("receiver failure has no terminal error")
	}
	assertGBLabFailureCleanup(t, h, lab, request.DeviceID, moduleSession, []string{
		fakeRTP, fakeRTCP, peerAddress,
		net.JoinHostPort("127.0.0.1", fmt.Sprint(modulePort)),
		net.JoinHostPort("127.0.0.1", fmt.Sprint(modulePort+1)),
	})
}

func TestGBLabReceiveSocketFailureTransitionsToFailedAndCleansUp(t *testing.T) {
	h := newRealGBLabHarness(t)
	stream, err := h.hub.GetOrCreate("gb28181/receive-socket-failure-source")
	if err != nil {
		t.Fatalf("GetOrCreate source: %v", err)
	}
	if err := stream.SetPublisher(&gbLabSourcePublisher{id: "receive-socket-failure-source", info: avframe.MediaInfo{
		VideoCodec: avframe.CodecH264, AudioCodec: avframe.CodecG711A, SampleRate: 8000, Channels: 1,
	}}); err != nil {
		t.Fatalf("SetPublisher source: %v", err)
	}
	request := LabSessionRequest{
		Mode:      LabModeReceive,
		DeviceID:  "34020000001320000131",
		ChannelID: "34020000001320000132",
		StreamKey: stream.Key(),
	}
	active, err := h.module.StartLabSession(context.Background(), request)
	if err != nil {
		t.Fatalf("StartLabSession receive: %v", err)
	}
	t.Cleanup(func() { _ = h.module.StopLabSession(active.ID) })
	lab := h.module.labs.session(active.ID)
	if lab == nil {
		t.Fatal("active receive Lab was not retained")
	}
	lab.mu.RLock()
	moduleSession := lab.moduleSession
	fakeRTP, fakeRTCP := lab.rtpConn.LocalAddr().String(), lab.rtcpConn.LocalAddr().String()
	peerAddress := lab.peerConn.LocalAddr().String()
	lab.mu.RUnlock()
	if moduleSession == nil || moduleSession.Snapshot().Sender == nil {
		t.Fatal("receive Lab has no module-owned outbound sender")
	}
	modulePort := moduleSession.Snapshot().LocalPort
	if err := lab.rtpConn.Close(); err != nil {
		t.Fatalf("close fake-device receive socket: %v", err)
	}
	stream.WriteFrame(deterministicGBLabTestFrame(0))

	failed := waitForGBLabSnapshot(t, h.module, active.ID, func(snapshot LabSessionSnapshot) bool {
		return snapshot.State == LabSessionStateFailed && strings.Contains(snapshot.LastError, "receive media")
	})
	if failed.LastError == "" {
		t.Fatal("receive socket failure has no terminal error")
	}
	if got := stream.Subscribers()["gb28181"]; got != 0 {
		t.Fatalf("receive socket failure left %d GB28181 subscribers", got)
	}
	assertGBLabFailureCleanup(t, h, lab, request.DeviceID, moduleSession, []string{
		fakeRTP, fakeRTCP, peerAddress,
		net.JoinHostPort("127.0.0.1", fmt.Sprint(modulePort)),
		net.JoinHostPort("127.0.0.1", fmt.Sprint(modulePort+1)),
	})
}

func assertGBLabFailureCleanup(t *testing.T, h realGBLabHarness, lab *gbLabSession, deviceID string, moduleSession *MediaSession, addresses []string) {
	t.Helper()
	if got := len(h.module.sessions.All()); got != 0 {
		t.Fatalf("Lab failure left %d module sessions", got)
	}
	if moduleSession != nil && h.module.sessions.Get(moduleSession.Snapshot().ID) != nil {
		t.Fatalf("Lab failure left module session %q registered", moduleSession.Snapshot().ID)
	}
	waitForGBLab(t, lab.byeReceived.Load)
	if h.module.registry.Get(deviceID) != nil {
		t.Fatal("Lab failure left fake device registered")
	}
	for _, address := range addresses {
		assertGBLabPortFree(t, address)
	}
	assertGBLabModulePortPairFree(t, h)
}

type gbLabSourcePublisher struct {
	id   string
	info avframe.MediaInfo
}

func (p *gbLabSourcePublisher) ID() string                    { return p.id }
func (p *gbLabSourcePublisher) MediaInfo() *avframe.MediaInfo { info := p.info; return &info }
func (p *gbLabSourcePublisher) Close() error                  { return nil }

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

func TestGBLabManagerBoundsTerminalHistoryAndReleasesReceiveResources(t *testing.T) {
	const historyLimit = 16
	h := newRealGBLabHarness(t)
	stream, err := h.hub.GetOrCreate("gb28181/history-source")
	if err != nil {
		t.Fatalf("GetOrCreate source: %v", err)
	}
	if err := stream.SetPublisher(&gbLabSourcePublisher{id: "history-source", info: avframe.MediaInfo{
		VideoCodec: avframe.CodecH264,
		AudioCodec: avframe.CodecG711A,
		SampleRate: 8000,
		Channels:   1,
	}}); err != nil {
		t.Fatalf("SetPublisher source: %v", err)
	}
	for timestamp := int64(0); timestamp < 200; timestamp += labmedia.AudioFrameDurationMs {
		if timestamp%labmedia.VideoFrameDurationMs == 0 {
			stream.WriteFrame(deterministicGBLabTestFrame(timestamp))
		}
		stream.WriteFrame(labmedia.G711Frame(avframe.CodecG711A, timestamp))
	}

	active, err := h.module.StartLabSession(context.Background(), LabSessionRequest{
		Mode:      LabModeReceive,
		DeviceID:  "34020000001329990001",
		ChannelID: "34020000001329990002",
		StreamKey: stream.Key(),
	})
	if err != nil {
		t.Fatalf("Start active receive session: %v", err)
	}
	t.Cleanup(func() { _ = h.module.StopLabSession(active.ID) })
	waitForGBLabSnapshot(t, h.module, active.ID, func(snapshot LabSessionSnapshot) bool {
		return snapshot.State == LabSessionStateActive && snapshot.AudioFramesRecv > 0 && snapshot.VideoFramesRecv > 0
	})

	terminalIDs := make([]string, 0, historyLimit+2)
	for i := 0; i < historyLimit+2; i++ {
		request := LabSessionRequest{
			Mode:      LabModeReceive,
			DeviceID:  fmt.Sprintf("3402000000132%07d", i*2+1),
			ChannelID: fmt.Sprintf("3402000000132%07d", i*2+2),
			StreamKey: stream.Key(),
		}
		session, err := h.module.StartLabSession(context.Background(), request)
		if err != nil {
			t.Fatalf("Start receive session %d: %v", i, err)
		}
		lab := h.module.labs.session(session.ID)
		if lab == nil {
			t.Fatalf("receive session %d was not retained", i)
		}
		lab.mu.RLock()
		clientUA, peerUA := lab.clientUA, lab.peerUA
		moduleSession := lab.moduleSession
		lab.mu.RUnlock()
		if clientUA == nil || peerUA == nil || clientUA == peerUA {
			t.Fatalf("receive session %d UAs = (%p, %p), want separate client and peer UAs", i, clientUA, peerUA)
		}
		fakeRTP, fakeRTCP := lab.mediaAddresses()
		peerAddress := lab.peerAddress()
		if moduleSession == nil {
			t.Fatalf("receive session %d has no module-owned egress", i)
		}
		modulePort := moduleSession.Snapshot().LocalPort
		waitForGBLabSnapshot(t, h.module, session.ID, func(snapshot LabSessionSnapshot) bool {
			return snapshot.State == LabSessionStateActive && snapshot.AudioFramesRecv > 0 && snapshot.VideoFramesRecv > 0
		})
		if err := h.module.StopLabSession(session.ID); err != nil {
			t.Fatalf("Stop receive session %d: %v", i, err)
		}
		assertGBLabPortFree(t, fakeRTP)
		assertGBLabPortFree(t, fakeRTCP)
		assertGBLabPortFree(t, peerAddress)
		assertGBLabPortFree(t, net.JoinHostPort("127.0.0.1", fmt.Sprint(modulePort)))
		assertGBLabPortFree(t, net.JoinHostPort("127.0.0.1", fmt.Sprint(modulePort+1)))
		if h.module.registry.Get(request.DeviceID) != nil {
			t.Fatalf("receive session %d left its fake device registered", i)
		}
		terminalIDs = append(terminalIDs, session.ID)
	}

	listed := h.module.ListLabSessions()
	if len(listed) != historyLimit+1 {
		t.Fatalf("GB lab sessions = %d, want one active plus %d terminal", len(listed), historyLimit)
	}
	states := make(map[string]LabSessionState, len(listed))
	for _, session := range listed {
		states[session.ID] = session.State
	}
	if states[active.ID] != LabSessionStateActive {
		t.Fatalf("active GB session state = %q, want retained active session", states[active.ID])
	}
	if _, ok := states[terminalIDs[0]]; ok {
		t.Fatal("oldest GB terminal session was not pruned")
	}
	if states[terminalIDs[len(terminalIDs)-1]] != LabSessionStateStopped {
		t.Fatal("newest GB terminal session was not retained")
	}
}

func TestGBLabTerminalErrorRedactsSecretsBeforeTruncation(t *testing.T) {
	err := errors.New(strings.Repeat("prefix ", 20) + "dialog sip:alice:topsecret@pbx.example.com\r\nAuthorization: Bearer secret-token " + strings.Repeat("suffix ", 30))
	message := redactedLabError(err)
	if strings.Contains(message, "topsecret") || strings.Contains(message, "secret-token") || strings.ContainsAny(message, "\r\n") {
		t.Fatalf("redacted terminal error leaked a secret or newline: %q", message)
	}
	if !strings.Contains(message, "[redacted]") {
		t.Fatalf("redacted terminal error = %q, want explicit redaction marker", message)
	}
	if len([]rune(message)) > 256 {
		t.Fatalf("redacted terminal error length = %d, want at most 256 runes", len([]rune(message)))
	}
}

type realGBLabHarness struct {
	module       *Module
	hub          *core.StreamHub
	rtpPortRange []int
}

func newRealGBLabHarness(t *testing.T) realGBLabHarness {
	return newRealGBLabHarnessWithConfig(t, nil)
}

func newRealGBLabHarnessWithConfig(t *testing.T, configure func(*config.Config)) realGBLabHarness {
	t.Helper()
	sipAddr := freeGBLabAddress(t)
	_, sipPort, err := sipAddress(sipAddr)
	if err != nil {
		t.Fatalf("parse real GB Lab SIP address: %v", err)
	}
	cfg := &config.Config{
		SIP: config.SIPConfig{
			Enabled:   true,
			Listen:    sipAddr,
			Transport: []string{"udp"},
			ServerID:  "liveforge-gb-lab",
			Domain:    "lab.local",
		},
		GB28181: config.GB28181Config{
			Enabled:      true,
			StreamPrefix: "gb28181",
			RTPPortRange: freeGBLabRTPPortRange(t, 2, []int{sipPort, sipPort}),
			Keepalive:    config.KeepaliveConfig{Timeout: time.Minute},
		},
		Stream: config.StreamConfig{GOPCache: true, GOPCacheNum: 1, RingBufferSize: 256},
	}
	if configure != nil {
		configure(cfg)
	}
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
	return realGBLabHarness{module: gbModule, hub: server.StreamHub(), rtpPortRange: append([]int(nil), cfg.GB28181.RTPPortRange...)}
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

func freeGBLabRTPPortRange(t *testing.T, pairCount int, excludedRanges ...[]int) []int {
	t.Helper()
	if pairCount <= 0 {
		t.Fatalf("RTP pair count = %d, want positive", pairCount)
	}
	for _, excluded := range excludedRanges {
		if len(excluded) != 2 || excluded[0] > excluded[1] {
			t.Fatalf("invalid excluded RTP range %v", excluded)
		}
	}

	const (
		minPort = 20000
		maxPort = 29999
	)
	portCount := pairCount * 2
	loopback := net.ParseIP("127.0.0.1")
	for start := minPort; start+portCount-1 <= maxPort; start += 2 {
		end := start + portCount - 1
		excluded := false
		for _, other := range excludedRanges {
			if start <= other[1] && other[0] <= end {
				excluded = true
				break
			}
		}
		if excluded {
			continue
		}

		reservations := make([]*net.UDPConn, 0, portCount)
		available := true
		for port := start; port <= end; port++ {
			conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: loopback, Port: port})
			if err != nil {
				available = false
				break
			}
			reservations = append(reservations, conn)
		}
		for _, conn := range reservations {
			_ = conn.Close()
		}
		if available {
			return []int{start, end}
		}
	}
	t.Fatalf("could not find %d contiguous RTP/RTCP pairs in %d-%d", pairCount, minPort, maxPort)
	return nil
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

func assertGBLabModulePortPairFree(t *testing.T, h realGBLabHarness) {
	t.Helper()
	rtpPort, rtcpPort, err := h.module.handler.ports.AllocatePair()
	if err != nil {
		t.Fatalf("allocate released module media pair: %v", err)
	}
	defer h.module.handler.ports.Free(rtpPort, rtcpPort)
	if len(h.rtpPortRange) != 2 || rtpPort != h.rtpPortRange[0] || rtcpPort != rtpPort+1 {
		t.Fatalf("released module pair = %d/%d, want first configured pair in %v", rtpPort, rtcpPort, h.rtpPortRange)
	}
	for _, port := range []int{rtpPort, rtcpPort} {
		conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: port})
		if err != nil {
			t.Fatalf("released module media port %d is still bound: %v", port, err)
		}
		_ = conn.Close()
	}
}
