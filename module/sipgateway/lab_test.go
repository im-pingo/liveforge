package sipgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/internal/labmedia"
	sipmod "github.com/im-pingo/liveforge/module/sip"
	"github.com/im-pingo/liveforge/pkg/avframe"
	mediarp "github.com/im-pingo/liveforge/pkg/rtp"
	"github.com/pion/rtcp"
	pionrtp "github.com/pion/rtp/v2"
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

func TestLabManagerRejectsAmbiguousStreamKeySegments(t *testing.T) {
	for _, streamKey := range []string{"/tenant/cam", "tenant/cam/", "tenant//cam", "tenant/./cam", "tenant/../cam", ".", ".."} {
		t.Run(streamKey, func(t *testing.T) {
			request := validSIPLabRequest()
			request.StreamKey = streamKey
			if err := validateLabRequest(request); !errors.Is(err, ErrLabInvalidRequest) {
				t.Fatalf("validateLabRequest(%q) error = %v, want ErrLabInvalidRequest", streamKey, err)
			}
		})
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

func TestLabManagerMarksTransportlessSessionAsContractOnly(t *testing.T) {
	manager := NewLabManager()
	session, err := manager.Start(context.Background(), validSIPLabRequest())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	const contractState LabSessionState = "contract"
	if session.State != contractState {
		t.Fatalf("transportless session state = %s, want %s", session.State, contractState)
	}
	if session.State == LabSessionStateActive {
		t.Fatal("transportless session was reported as active")
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

func TestSIPLabManagerEnforcesActiveSessionCeiling(t *testing.T) {
	manager := newLabManagerWithLimit(nil, 1)
	firstRequest := validSIPLabRequest()
	firstRequest.DeviceID = "ceiling-device-1"
	first, err := manager.Start(context.Background(), firstRequest)
	if err != nil {
		t.Fatalf("first Start: %v", err)
	}
	secondRequest := validSIPLabRequest()
	secondRequest.DeviceID = "ceiling-device-2"
	if _, err := manager.Start(context.Background(), secondRequest); !errors.Is(err, ErrLabCapacity) {
		t.Fatalf("second Start error = %v, want ErrLabCapacity", err)
	}
	if err := manager.Stop(first.ID); err != nil {
		t.Fatalf("Stop first session: %v", err)
	}
	if _, err := manager.Start(context.Background(), secondRequest); err != nil {
		t.Fatalf("Start after terminal session: %v", err)
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

func TestSIPLabManagerBoundsTerminalHistoryWithoutPruningActive(t *testing.T) {
	const historyLimit = 16
	manager := newLabManager(nil)
	activeRequest := validSIPLabRequest()
	activeRequest.DeviceID = "active-history-device"
	active, err := manager.Start(context.Background(), activeRequest)
	if err != nil {
		t.Fatalf("Start active session: %v", err)
	}
	terminalIDs := make([]string, 0, historyLimit+2)
	for i := 0; i < historyLimit+2; i++ {
		request := validSIPLabRequest()
		request.DeviceID = fmt.Sprintf("terminal-history-device-%d", i)
		session, err := manager.Start(context.Background(), request)
		if err != nil {
			t.Fatalf("Start terminal session %d: %v", i, err)
		}
		if err := manager.Stop(session.ID); err != nil {
			t.Fatalf("Stop terminal session %d: %v", i, err)
		}
		terminalIDs = append(terminalIDs, session.ID)
	}

	listed := manager.List()
	if len(listed) != historyLimit+1 {
		t.Fatalf("lab sessions = %d, want one active plus %d terminal", len(listed), historyLimit)
	}
	states := make(map[string]LabSessionState, len(listed))
	for _, session := range listed {
		states[session.ID] = session.State
	}
	if states[active.ID] != LabSessionStateContract {
		t.Fatalf("active session state = %q, want retained contract session", states[active.ID])
	}
	if _, ok := states[terminalIDs[0]]; ok {
		t.Fatal("oldest terminal session was not pruned")
	}
	if states[terminalIDs[len(terminalIDs)-1]] != LabSessionStateStopped {
		t.Fatal("newest terminal session was not retained")
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
			snapshot.AudioRTPPacketsSent > 0 && snapshot.VideoRTPPacketsSent > 0 &&
			snapshot.RTCPPacketsSent > 0
	})
	if active.Direction != LabDirectionInbound || active.Codec != "PCMA" {
		t.Fatalf("publish snapshot = %+v, want inbound PCMA", active)
	}
	stream, ok := h.hub.Find(request.StreamKey)
	if !ok || stream.Publisher() == nil {
		t.Fatalf("publish stream = (%v, %v), want active real publisher", stream, ok)
	}
	waitForSIPLab(t, func() bool {
		stats := stream.Stats()
		gop := stream.GOPCacheDetail()
		return stats.VideoFrames >= 10 && stats.AudioFrames >= 10 &&
			gop.VideoFrames >= 2 && gop.AudioFrames >= 2 && gop.DurationMs >= 40
	})
	mediaInfo := stream.Publisher().MediaInfo()
	if mediaInfo == nil || mediaInfo.VideoCodec != avframe.CodecH264 || mediaInfo.AudioCodec != avframe.CodecG711A {
		t.Fatalf("publish media info = %+v, want H264/G711A", mediaInfo)
	}
	sps, pps := mediarp.ExtractSPSPPS(mediaInfo.VideoSequenceHeader)
	if len(sps) == 0 || len(pps) == 0 {
		t.Fatalf("publish media info has no decodable H.264 parameter sets: %x", mediaInfo.VideoSequenceHeader)
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

func TestSIPLabPublishUsesBoundGatewayRTCPReceivers(t *testing.T) {
	h := newRealSIPLabHarness(t)
	session, err := h.module.StartLabSession(context.Background(), LabSessionRequest{
		Mode:      LabModePublish,
		DeviceID:  "rtcp-receiver-device",
		StreamKey: "sip/rtcp-receiver",
		Codec:     "PCMA",
	})
	if err != nil {
		t.Fatalf("StartLabSession publish: %v", err)
	}
	t.Cleanup(func() { _ = h.module.StopLabSession(session.ID) })
	waitForSIPLabSnapshot(t, h.module, session.ID, func(snapshot LabSessionSnapshot) bool {
		return snapshot.State == LabSessionStateActive && snapshot.RTCPPacketsSent >= 2
	})

	h.module.Gateway().labs.mu.RLock()
	lab := h.module.Gateway().labs.sessions[session.ID]
	h.module.Gateway().labs.mu.RUnlock()
	if lab == nil {
		t.Fatal("publish lab session was not retained")
	}
	lab.mu.RLock()
	callID := lab.callID
	lab.mu.RUnlock()
	call, ok := h.module.Call(callID)
	if !ok {
		t.Fatalf("gateway call %q was not found", callID)
	}
	for _, port := range []int{call.RTCPPort, call.VideoRTCPPort} {
		probe, bindErr := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: port})
		if bindErr == nil {
			_ = probe.Close()
			t.Fatalf("gateway RTCP port %d is not bound", port)
		}
	}

	if err := h.module.StopLabSession(session.ID); err != nil {
		t.Fatalf("StopLabSession publish: %v", err)
	}
	terminalLab := waitForSIPLabSnapshot(t, h.module, session.ID, func(snapshot LabSessionSnapshot) bool {
		return snapshot.State == LabSessionStateStopped
	})
	terminalCall, ok := h.module.Call(callID)
	if !ok {
		t.Fatalf("terminal gateway call %q was not retained", callID)
	}
	encoded, err := json.Marshal(terminalCall)
	if err != nil {
		t.Fatalf("marshal terminal call: %v", err)
	}
	var counters map[string]any
	if err := json.Unmarshal(encoded, &counters); err != nil {
		t.Fatalf("decode terminal call counters: %v", err)
	}
	received, ok := counters["rtcp_packets_received"].(float64)
	if !ok || received < 2 {
		t.Fatalf("terminal call counters = %s, want parsed RTCP for both tracks", encoded)
	}
	if terminalLab.RTCPPacketsSent != uint64(received) {
		t.Fatalf("lab RTCP sent = %d, gateway parsed = %.0f; want receiver-side accounting", terminalLab.RTCPPacketsSent, received)
	}
}

func TestSIPLabPublishAcceptsUnprefixedStreamKey(t *testing.T) {
	h := newRealSIPLabHarness(t)
	request := LabSessionRequest{
		Mode:      LabModePublish,
		DeviceID:  "unprefixed-publish-device",
		StreamKey: "s1",
		Codec:     "PCMA",
	}

	session, err := h.module.StartLabSession(context.Background(), request)
	if err != nil {
		t.Fatalf("StartLabSession publish: %v", err)
	}
	t.Cleanup(func() { _ = h.module.StopLabSession(session.ID) })

	active := waitForSIPLabSnapshot(t, h.module, session.ID, func(snapshot LabSessionSnapshot) bool {
		return snapshot.State == LabSessionStateActive && snapshot.RTPPacketsSent > 0
	})
	if active.StreamKey != request.StreamKey {
		t.Fatalf("active stream key = %q, want %q", active.StreamKey, request.StreamKey)
	}
	stream, ok := h.hub.Find(request.StreamKey)
	if !ok || stream.Publisher() == nil {
		t.Fatalf("publish stream = (%v, %v), want active publisher", stream, ok)
	}
}

func TestSIPLabStreamKeyOverrideRequiresLoopback(t *testing.T) {
	request := sip.NewRequest(sip.INVITE, sip.Uri{Scheme: "sip", User: "channel", Host: "lab.local"})
	request.AppendHeader(sip.NewHeader("X-LiveForge-Lab-Stream-Key", "s1"))
	gateway := &Gateway{prefix: "sip"}

	request.SetSource("127.0.0.1:5060")
	if got := gateway.streamKeyFromRequest(request); got != "s1" {
		t.Fatalf("loopback stream key = %q, want s1", got)
	}

	request.SetSource("192.0.2.10:5060")
	if got := gateway.streamKeyFromRequest(request); got != "sip/channel" {
		t.Fatalf("non-loopback stream key = %q, want sip/channel", got)
	}
}

func TestSIPLabGatewayCloseCancelsBlockedStart(t *testing.T) {
	target := freeSIPLabUDPAddress(t)
	svc := &mockSIPService{localAddr: target, serverID: "test", domain: "test.local"}
	gw, err := NewGateway(newTestGatewayConfig(t), svc, newTestHub(), core.NewEventBus())
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}

	request := LabSessionRequest{
		Mode:      LabModePublish,
		DeviceID:  "blocked-start-device",
		StreamKey: "sip/blocked-start",
		Codec:     "PCMA",
	}
	startDone := make(chan error, 1)
	go func() {
		_, startErr := gw.StartLabSession(context.Background(), request)
		startDone <- startErr
	}()

	var session *sipLabSession
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		gw.labs.mu.RLock()
		for _, candidate := range gw.labs.sessions {
			if candidate.identity == request.DeviceID {
				session = candidate
				break
			}
		}
		gw.labs.mu.RUnlock()
		if session != nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if session == nil {
		t.Fatal("blocked lab start did not create a session")
	}
	var rtpAddr, rtcpAddr net.UDPAddr
	deadline = time.Now().Add(time.Second)
	socketsCaptured := false
	for time.Now().Before(deadline) {
		session.mu.RLock()
		rtpConn, rtcpConn := session.rtpConn, session.rtcpConn
		if rtpConn != nil && rtcpConn != nil {
			rtpAddr = *rtpConn.LocalAddr().(*net.UDPAddr)
			rtcpAddr = *rtcpConn.LocalAddr().(*net.UDPAddr)
			socketsCaptured = true
			session.mu.RUnlock()
			break
		}
		session.mu.RUnlock()
		time.Sleep(time.Millisecond)
	}
	if !socketsCaptured {
		t.Fatal("blocked lab start did not allocate its RTP/RTCP sockets")
	}

	closeDone := make(chan struct{})
	go func() {
		gw.Close()
		close(closeDone)
	}()
	select {
	case <-closeDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Gateway.Close blocked behind an unanswered Background-context lab INVITE")
	}
	select {
	case startErr := <-startDone:
		if startErr == nil {
			t.Fatal("blocked lab start returned nil error")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("lab start goroutine remained blocked after Gateway.Close")
	}

	snapshot := session.snapshot()
	if snapshot.State != LabSessionStateStopped {
		t.Fatalf("blocked lab session state = %s, want stopped", snapshot.State)
	}
	for _, addr := range []*net.UDPAddr{&rtpAddr, &rtcpAddr} {
		conn, bindErr := net.ListenUDP("udp4", addr)
		if bindErr != nil {
			t.Fatalf("rebind released lab UDP socket %s: %v", addr, bindErr)
		}
		_ = conn.Close()
	}
}

func TestSIPLabBYEConsumesProvisionalResponseBeforeFinal(t *testing.T) {
	ua, err := sipgo.NewUA()
	if err != nil {
		t.Fatalf("NewUA: %v", err)
	}
	t.Cleanup(func() { _ = ua.Close() })
	client, err := sipgo.NewClient(ua)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	tx := &offeredFinalClientTx{
		responses: make(chan *sip.Response, 2),
		done:      make(chan struct{}),
	}
	client.TxRequester = labClientTxRequesterFunc(func(context.Context, *sip.Request) (sip.ClientTransaction, error) {
		tx.responses <- sip.NewResponse(100, "Trying")
		tx.responses <- sip.NewResponse(200, "OK")
		return tx, nil
	})

	invite := newLabInvite(sip.INVITE, sip.Uri{Scheme: "sip", User: "device", Host: "lab.local"}, "device", "lab.local", "bye-regression")
	response := sip.NewResponseFromRequest(invite, 200, "OK", nil)
	if err := sendLabBYE(context.Background(), client, invite, response); err != nil {
		t.Fatalf("sendLabBYE returned false error for provisional response: %v", err)
	}
}

type labClientTxRequesterFunc func(context.Context, *sip.Request) (sip.ClientTransaction, error)

func (f labClientTxRequesterFunc) Request(ctx context.Context, req *sip.Request) (sip.ClientTransaction, error) {
	return f(ctx, req)
}

func TestSIPLabReceiveAcceptsGatewayInviteAndCleansUp(t *testing.T) {
	h := newRealSIPLabHarness(t)
	stream, err := h.hub.GetOrCreate("sip/persistent-receive")
	if err != nil {
		t.Fatalf("GetOrCreate receive stream: %v", err)
	}
	if err := stream.SetPublisher(&gatewayTestPublisher{
		id: "sip-lab-av-source",
		info: &avframe.MediaInfo{
			VideoCodec:          avframe.CodecH264,
			VideoSequenceHeader: labmedia.VideoFrame(0).Payload,
			AudioCodec:          avframe.CodecG711U,
			SampleRate:          8000,
			Channels:            1,
		},
	}); err != nil {
		t.Fatalf("SetPublisher receive source: %v", err)
	}
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
	for timestamp := int64(0); timestamp < 200; timestamp += 20 {
		if timestamp%40 == 0 {
			stream.WriteFrame(labmedia.VideoFrame(timestamp))
		}
		stream.WriteFrame(labmedia.G711Frame(avframe.CodecG711U, timestamp))
	}
	active := waitForSIPLabSnapshot(t, h.module, session.ID, func(snapshot LabSessionSnapshot) bool {
		return snapshot.State == LabSessionStateActive &&
			snapshot.AudioRTPPacketsRecv > 0 && snapshot.VideoRTPPacketsRecv > 0 &&
			snapshot.RTCPPacketsRecv > 0
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

func TestSIPOutboundSubscriberAdmissionFailsBeforeActivation(t *testing.T) {
	hub := core.NewStreamHub(
		config.StreamConfig{RingBufferSize: 16},
		config.LimitsConfig{MaxSubscribersPerStream: 1},
		core.NewEventBus(),
	)
	stream, err := hub.GetOrCreate("sip/admission-source")
	if err != nil {
		t.Fatalf("GetOrCreate source: %v", err)
	}
	if err := stream.SetPublisher(&gatewayTestPublisher{id: "admission-source", info: &avframe.MediaInfo{
		AudioCodec: avframe.CodecG711A,
		SampleRate: 8000,
		Channels:   1,
	}}); err != nil {
		t.Fatalf("SetPublisher source: %v", err)
	}
	if err := stream.AddSubscriber("occupied"); err != nil {
		t.Fatalf("occupy subscriber capacity: %v", err)
	}
	defer stream.RemoveSubscriber("occupied")

	remoteRTP, remoteRTCP, err := listenLabUDPPair()
	if err != nil {
		t.Fatalf("listen remote RTP pair: %v", err)
	}
	defer remoteRTP.Close()
	defer remoteRTCP.Close()
	localRTP, localRTCP, err := listenLabUDPPair()
	if err != nil {
		t.Fatalf("reserve local RTP pair: %v", err)
	}
	localRTPPort := localRTP.LocalAddr().(*net.UDPAddr).Port
	localRTCPPort := localRTCP.LocalAddr().(*net.UDPAddr).Port

	call := newCallSession("admission-call", stream.Key(), negotiatedCodec{
		Codec: avframe.CodecG711A, PT: 8, ClockRate: 8000, EncodingName: "PCMA",
	}, "outbound", localRTPPort, localRTCPPort)
	call.configureMediaSockets(localRTP, localRTCP)
	defer call.Close()
	err = call.startOutbound(stream, stream.StartupSnapshot(), "127.0.0.1", remoteRTP.LocalAddr().(*net.UDPAddr).Port)
	if err == nil || !strings.Contains(err.Error(), "max subscribers per stream") {
		t.Fatalf("startOutbound admission error = %v, want synchronous subscriber limit error", err)
	}
	if snapshot := call.snapshot(); snapshot.State == CallStateActive {
		t.Fatalf("subscriber-rejected call state = %q, must never become active", snapshot.State)
	}
	if got := stream.Subscribers()["sipgateway"]; got != 0 {
		t.Fatalf("subscriber rejection left %d SIP gateway subscribers", got)
	}
}

func TestSIPLabPublishCallFailureTransitionsToFailedAndCleansUp(t *testing.T) {
	h := newRealSIPLabHarness(t)
	request := LabSessionRequest{
		Mode:      LabModePublish,
		DeviceID:  "publish-call-failure-device",
		StreamKey: "sip/publish-call-failure",
		Codec:     "PCMA",
	}
	session, err := h.module.StartLabSession(context.Background(), request)
	if err != nil {
		t.Fatalf("StartLabSession publish: %v", err)
	}
	t.Cleanup(func() { _ = h.module.StopLabSession(session.ID) })
	lab, call := activeSIPLabCall(t, h.module.Gateway(), session.ID)
	addresses := sipLabMediaAddresses(lab)
	call.networkLost(errors.New("forced publish call failure"))

	failed := waitForSIPLabSnapshot(t, h.module, session.ID, func(snapshot LabSessionSnapshot) bool {
		return snapshot.State == LabSessionStateFailed
	})
	if !strings.Contains(failed.LastError, "forced publish call failure") {
		t.Fatalf("publish Lab terminal error = %q", failed.LastError)
	}
	waitForSIPLab(t, func() bool {
		stream, ok := h.hub.Find(request.StreamKey)
		return h.module.Gateway().ActiveCalls() == 0 && ok && stream.Publisher() == nil
	})
	for _, address := range addresses {
		assertSIPLabPortFree(t, address)
	}
}

func TestSIPLabReceiveCallFailureTransitionsToFailedAndCleansUp(t *testing.T) {
	h := newRealSIPLabHarness(t)
	stream, err := h.hub.GetOrCreate("sip/receive-call-failure")
	if err != nil {
		t.Fatalf("GetOrCreate source: %v", err)
	}
	publisher := &gatewayTestPublisher{id: "receive-call-failure-source", info: &avframe.MediaInfo{
		VideoCodec:          avframe.CodecH264,
		VideoSequenceHeader: labmedia.VideoFrame(0).Payload,
		AudioCodec:          avframe.CodecG711A,
		SampleRate:          8000,
		Channels:            1,
	}}
	if err := stream.SetPublisher(publisher); err != nil {
		t.Fatalf("SetPublisher source: %v", err)
	}
	session, err := h.module.StartLabSession(context.Background(), LabSessionRequest{
		Mode: LabModeReceive, DeviceID: "receive-call-failure-device", StreamKey: stream.Key(), Codec: "PCMA",
	})
	if err != nil {
		t.Fatalf("StartLabSession receive: %v", err)
	}
	t.Cleanup(func() { _ = h.module.StopLabSession(session.ID) })
	lab, call := activeSIPLabCall(t, h.module.Gateway(), session.ID)
	addresses := sipLabMediaAddresses(lab)
	call.networkLost(errors.New("forced receive call failure"))

	failed := waitForSIPLabSnapshot(t, h.module, session.ID, func(snapshot LabSessionSnapshot) bool {
		return snapshot.State == LabSessionStateFailed
	})
	if !strings.Contains(failed.LastError, "forced receive call failure") {
		t.Fatalf("receive Lab terminal error = %q", failed.LastError)
	}
	if stream.Publisher() != publisher {
		t.Fatal("receive Lab failure removed or replaced the source publisher")
	}
	if got := stream.Subscribers()["sipgateway"]; got != 0 {
		t.Fatalf("receive Lab failure left %d SIP gateway subscribers", got)
	}
	for _, address := range addresses {
		assertSIPLabPortFree(t, address)
	}
}

func TestSIPLabReceiveDoesNotMutateSourceStream(t *testing.T) {
	h := newRealSIPLabHarness(t)
	stream, err := h.hub.GetOrCreate("sip/immutable-receive-source")
	if err != nil {
		t.Fatalf("GetOrCreate receive stream: %v", err)
	}
	if err := stream.SetPublisher(&gatewayTestPublisher{
		id: "dedicated-receive-source",
		info: &avframe.MediaInfo{
			VideoCodec:          avframe.CodecH264,
			VideoSequenceHeader: labmedia.VideoFrame(0).Payload,
			AudioCodec:          avframe.CodecG711A,
			SampleRate:          8000,
			Channels:            1,
		},
	}); err != nil {
		t.Fatalf("SetPublisher receive source: %v", err)
	}
	beforeStats := stream.Stats()
	beforeCache := stream.GOPCacheDetail()

	session, err := h.module.StartLabSession(context.Background(), LabSessionRequest{
		Mode:      LabModeReceive,
		DeviceID:  "immutable-receive-device",
		StreamKey: stream.Key(),
		Codec:     "PCMA",
	})
	if err != nil {
		t.Fatalf("StartLabSession receive: %v", err)
	}
	t.Cleanup(func() { _ = h.module.StopLabSession(session.ID) })
	time.Sleep(120 * time.Millisecond)

	afterStats := stream.Stats()
	afterCache := stream.GOPCacheDetail()
	if afterStats.BytesIn != beforeStats.BytesIn ||
		afterStats.AudioFrames != beforeStats.AudioFrames ||
		afterStats.VideoFrames != beforeStats.VideoFrames ||
		afterCache.AudioFrames != beforeCache.AudioFrames ||
		afterCache.VideoFrames != beforeCache.VideoFrames {
		t.Fatalf("receive lab mutated source: stats before=%+v after=%+v cache before=%+v after=%+v", beforeStats, afterStats, beforeCache, afterCache)
	}
}

func TestSIPLabReceiveSendsPeriodicPerTrackReceiverReports(t *testing.T) {
	h := newRealSIPLabHarness(t)
	const idleTimeout = 100 * time.Millisecond
	h.module.Gateway().rtpIdleTimeout = idleTimeout
	stream, err := h.hub.GetOrCreate("sip/periodic-receiver-reports")
	if err != nil {
		t.Fatalf("GetOrCreate receive stream: %v", err)
	}
	if err := stream.SetPublisher(&gatewayTestPublisher{
		id: "periodic-rr-source",
		info: &avframe.MediaInfo{
			VideoCodec:          avframe.CodecH264,
			VideoSequenceHeader: labmedia.VideoFrame(0).Payload,
			AudioCodec:          avframe.CodecG711A,
			SampleRate:          8000,
			Channels:            1,
		},
	}); err != nil {
		t.Fatalf("SetPublisher receive source: %v", err)
	}

	session, err := h.module.StartLabSession(context.Background(), LabSessionRequest{
		Mode:      LabModeReceive,
		DeviceID:  "periodic-rr-device",
		StreamKey: stream.Key(),
		Codec:     "PCMA",
	})
	if err != nil {
		t.Fatalf("StartLabSession receive: %v", err)
	}
	t.Cleanup(func() { _ = h.module.StopLabSession(session.ID) })
	stopSource := make(chan struct{})
	defer close(stopSource)
	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		var timestamp int64
		for {
			if timestamp%40 == 0 {
				stream.WriteFrame(labmedia.VideoFrame(timestamp))
			}
			stream.WriteFrame(labmedia.G711Frame(avframe.CodecG711A, timestamp))
			timestamp += 20
			select {
			case <-stopSource:
				return
			case <-ticker.C:
			}
		}
	}()

	time.Sleep(4 * idleTimeout)
	h.module.Gateway().labs.mu.RLock()
	lab := h.module.Gateway().labs.sessions[session.ID]
	h.module.Gateway().labs.mu.RUnlock()
	if lab == nil {
		t.Fatal("receive lab session was not retained")
	}
	lab.mu.RLock()
	callID := lab.callID
	lab.mu.RUnlock()
	call, ok := h.module.Call(callID)
	if !ok || call.State != CallStateActive {
		t.Fatalf("call after four RTCP idle windows = (%+v, %v), want active", call, ok)
	}
	snapshot := waitForSIPLabSnapshot(t, h.module, session.ID, func(snapshot LabSessionSnapshot) bool {
		return snapshot.RTCPPacketsSent >= 4
	})
	if snapshot.RTCPPacketsSent < 4 {
		t.Fatalf("receiver reports sent = %d, want periodic reports for audio and video", snapshot.RTCPPacketsSent)
	}
}

func TestSIPOutboundSenderReportsArePerTrackRFCCompliantAndPeriodic(t *testing.T) {
	audioRTP, audioRTCP, err := listenLabUDPPair()
	if err != nil {
		t.Fatalf("listen audio pair: %v", err)
	}
	defer audioRTP.Close()
	defer audioRTCP.Close()
	videoRTP, videoRTCP, err := listenLabUDPPair()
	if err != nil {
		t.Fatalf("listen video pair: %v", err)
	}
	defer videoRTP.Close()
	defer videoRTCP.Close()
	audioSendRTP, audioSendRTCP, err := listenLabUDPPair()
	if err != nil {
		t.Fatalf("listen audio sender pair: %v", err)
	}
	defer audioSendRTP.Close()
	defer audioSendRTCP.Close()
	videoSendRTP, videoSendRTCP, err := listenLabUDPPair()
	if err != nil {
		t.Fatalf("listen video sender pair: %v", err)
	}
	defer videoSendRTP.Close()
	defer videoSendRTCP.Close()

	audioCodec := negotiatedCodec{Codec: avframe.CodecG711A, PT: 8, ClockRate: 8000, EncodingName: "PCMA"}
	call := newCallSession("sender-report-test", "sip/source", audioCodec, "outbound", 0, 0)
	audioSession := mediarp.NewSession(uint8(audioCodec.PT), uint32(audioCodec.ClockRate))
	audioPacketizer, err := mediarp.NewPacketizer(avframe.CodecG711A)
	if err != nil {
		t.Fatalf("new audio packetizer: %v", err)
	}
	videoSession := mediarp.NewSession(uint8(sipH264Codec.PT), uint32(sipH264Codec.ClockRate))
	videoPacketizer, err := mediarp.NewPacketizer(avframe.CodecH264)
	if err != nil {
		t.Fatalf("new video packetizer: %v", err)
	}
	var audioReportState, videoReportState rtcpSenderState
	if !call.sendFrame(labmedia.G711Frame(avframe.CodecG711A, 0), audioPacketizer, audioSession, audioSendRTP, audioSendRTCP, audioRTP.LocalAddr().(*net.UDPAddr), &audioReportState) {
		t.Fatal("send audio frame failed")
	}
	audioPacket := readSIPLabRTPPacket(t, audioRTP)
	audioReport := readSIPLabSenderReport(t, audioRTCP)
	assertSIPLabSenderReport(t, audioReport, audioPacket, 1, uint32(len(audioPacket.Payload)))

	if !call.sendFrame(labmedia.VideoFrame(0), videoPacketizer, videoSession, videoSendRTP, videoSendRTCP, videoRTP.LocalAddr().(*net.UDPAddr), &videoReportState) {
		t.Fatal("send video frame failed")
	}
	videoPacket := readSIPLabRTPPacket(t, videoRTP)
	videoReport := readSIPLabSenderReport(t, videoRTCP)
	assertSIPLabSenderReport(t, videoReport, videoPacket, 1, uint32(len(videoPacket.Payload)))

	time.Sleep(1100 * time.Millisecond)
	if !call.sendFrame(labmedia.G711Frame(avframe.CodecG711A, 20), audioPacketizer, audioSession, audioSendRTP, audioSendRTCP, audioRTP.LocalAddr().(*net.UDPAddr), &audioReportState) {
		t.Fatal("send second audio frame failed")
	}
	secondAudioPacket := readSIPLabRTPPacket(t, audioRTP)
	secondAudioReport := readSIPLabSenderReport(t, audioRTCP)
	assertSIPLabSenderReport(t, secondAudioReport, secondAudioPacket, 2, uint32(len(audioPacket.Payload)+len(secondAudioPacket.Payload)))
}

func readSIPLabRTPPacket(t *testing.T, conn *net.UDPConn) *pionrtp.Packet {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond)); err != nil {
		t.Fatalf("set RTP deadline: %v", err)
	}
	buf := make([]byte, 2048)
	n, _, err := conn.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("read RTP packet: %v", err)
	}
	var packet pionrtp.Packet
	if err := packet.Unmarshal(buf[:n]); err != nil {
		t.Fatalf("unmarshal RTP packet: %v", err)
	}
	return &packet
}

func readSIPLabSenderReport(t *testing.T, conn *net.UDPConn) *rtcp.SenderReport {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond)); err != nil {
		t.Fatalf("set RTCP deadline: %v", err)
	}
	buf := make([]byte, 2048)
	n, _, err := conn.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("read RTCP sender report: %v", err)
	}
	packets, err := rtcp.Unmarshal(buf[:n])
	if err != nil || len(packets) != 1 {
		t.Fatalf("unmarshal RTCP sender report = (%v, %v)", packets, err)
	}
	report, ok := packets[0].(*rtcp.SenderReport)
	if !ok {
		t.Fatalf("RTCP packet = %T, want *rtcp.SenderReport", packets[0])
	}
	return report
}

func assertSIPLabSenderReport(t *testing.T, report *rtcp.SenderReport, packet *pionrtp.Packet, packetCount, octetCount uint32) {
	t.Helper()
	if report.SSRC != packet.SSRC {
		t.Fatalf("sender report SSRC = %#x, RTP SSRC = %#x", report.SSRC, packet.SSRC)
	}
	const ntpEpochOffset = 2208988800
	reportTime := time.Unix(int64(report.NTPTime>>32)-ntpEpochOffset, 0)
	if delta := time.Since(reportTime); delta < -time.Second || delta > 2*time.Second {
		t.Fatalf("sender report NTP time = %d (%s), want RFC NTP near now", report.NTPTime, reportTime)
	}
	if report.RTPTime != packet.Timestamp || report.PacketCount != packetCount || report.OctetCount != octetCount {
		t.Fatalf("sender report = %+v, RTP timestamp=%d payload=%d; want packets=%d octets=%d", report, packet.Timestamp, len(packet.Payload), packetCount, octetCount)
	}
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

func TestSIPLabHarnessRTPRangeExcludesSIPListener(t *testing.T) {
	sipAddr := freeSIPLabUDPAddress(t)
	_, sipPortText, err := net.SplitHostPort(sipAddr)
	if err != nil {
		t.Fatalf("split SIP address: %v", err)
	}
	sipPort, err := strconv.Atoi(sipPortText)
	if err != nil {
		t.Fatalf("parse SIP port: %v", err)
	}

	rtpRange := freeSIPLabRTPPortRange(t, sipPort)
	if len(rtpRange) != 2 || rtpRange[0]%2 != 0 || rtpRange[1] != rtpRange[0]+15 {
		t.Fatalf("RTP range = %v, want eight even-aligned RTP/RTCP pairs", rtpRange)
	}
	if sipPort >= rtpRange[0] && sipPort <= rtpRange[1] {
		t.Fatalf("SIP control port %d overlaps RTP range %v", sipPort, rtpRange)
	}

	reservations := make([]*net.UDPConn, 0, 4)
	defer func() {
		for _, conn := range reservations {
			_ = conn.Close()
		}
	}()
	for port := rtpRange[0]; port <= rtpRange[1]; port++ {
		conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: port})
		if err != nil {
			t.Fatalf("RTP range port %d is not free: %v", port, err)
		}
		reservations = append(reservations, conn)
	}
}

func TestSIPLabMediaPortsExcludeGatewayRTPRange(t *testing.T) {
	gw := &Gateway{rtpPortMin: 1, rtpPortMax: 65535}

	rtpConn, rtcpConn, err := gw.listenLabUDPPair()
	if err == nil {
		_ = rtpConn.Close()
		_ = rtcpConn.Close()
		t.Fatal("listenLabUDPPair succeeded inside an excluded gateway RTP range")
	}
	if rtpConn != nil || rtcpConn != nil {
		t.Fatalf("failed Lab pair = (%v, %v), want no retained sockets", rtpConn, rtcpConn)
	}
}

func newRealSIPLabHarness(t *testing.T) realSIPLabHarness {
	t.Helper()
	sipAddr := freeSIPLabUDPAddress(t)
	_, sipPortText, err := net.SplitHostPort(sipAddr)
	if err != nil {
		t.Fatalf("split SIP address: %v", err)
	}
	sipPort, err := strconv.Atoi(sipPortText)
	if err != nil {
		t.Fatalf("parse SIP port: %v", err)
	}
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
				RTPPortRange: freeSIPLabRTPPortRange(t, sipPort),
				Codecs:       []string{"PCMA", "PCMU"},
				MaxCalls:     8,
			},
		},
		Stream: config.StreamConfig{GOPCache: true, GOPCacheNum: 1, RingBufferSize: 256},
	}
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

func freeSIPLabRTPPortRange(t *testing.T, excludedPort int) []int {
	t.Helper()
	const (
		portCount   = 16
		maxAttempts = 128
	)
	loopback := net.ParseIP("127.0.0.1")
	for attempt := 0; attempt < maxAttempts; attempt++ {
		probe, err := net.ListenUDP("udp4", &net.UDPAddr{IP: loopback})
		if err != nil {
			t.Fatalf("probe RTP range: %v", err)
		}
		start := probe.LocalAddr().(*net.UDPAddr).Port
		_ = probe.Close()
		if start%2 != 0 {
			start--
		}
		end := start + portCount - 1
		if start < 1024 || end > 65535 || excludedPort >= start && excludedPort <= end {
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
	t.Fatalf("could not find eight free RTP/RTCP pairs excluding SIP port %d", excludedPort)
	return nil
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
	if port+100 > 65535 {
		port = 60000
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

func activeSIPLabCall(t *testing.T, gateway *Gateway, labID string) (*sipLabSession, *CallSession) {
	t.Helper()
	gateway.labs.mu.RLock()
	lab := gateway.labs.sessions[labID]
	gateway.labs.mu.RUnlock()
	if lab == nil {
		t.Fatalf("SIP Lab %q was not retained", labID)
	}
	lab.mu.RLock()
	callID := lab.callID
	lab.mu.RUnlock()
	gateway.mu.RLock()
	call := gateway.sessions[callID]
	gateway.mu.RUnlock()
	if call == nil {
		t.Fatalf("SIP Lab call %q is not active", callID)
	}
	return lab, call
}

func sipLabMediaAddresses(lab *sipLabSession) []string {
	lab.mu.RLock()
	defer lab.mu.RUnlock()
	addresses := make([]string, 0, 4)
	for _, conn := range []*net.UDPConn{lab.rtpConn, lab.rtcpConn, lab.videoRTPConn, lab.videoRTCPConn} {
		if conn != nil {
			addresses = append(addresses, conn.LocalAddr().String())
		}
	}
	return addresses
}

func assertSIPLabPortFree(t *testing.T, address string) {
	t.Helper()
	if address == "" {
		return
	}
	addr, err := net.ResolveUDPAddr("udp4", address)
	if err != nil {
		t.Fatalf("resolve UDP address %q: %v", address, err)
	}
	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		t.Fatalf("UDP address %q was not released: %v", address, err)
	}
	_ = conn.Close()
}
