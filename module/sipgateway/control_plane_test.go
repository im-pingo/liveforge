package sipgateway

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	sipmod "github.com/im-pingo/liveforge/module/sip"
	"github.com/im-pingo/liveforge/pkg/avframe"
	pionrtp "github.com/pion/rtp/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

const testAudioOffer = "v=0\r\no=- 1 1 IN IP4 127.0.0.1\r\ns=-\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\nm=audio 49998 RTP/AVP 8\r\na=rtpmap:8 PCMA/8000\r\n"

func inviteGateway(t *testing.T, svc *mockSIPService, callID, stream string, body []byte) *sip.Response {
	t.Helper()
	req := sip.NewRequest(sip.INVITE, sip.Uri{User: stream, Host: "test.local"})
	req.AppendHeader(sip.NewHeader("Call-ID", callID))
	req.SetBody(body)
	tx := &mockServerTx{}

	svc.mu.Lock()
	handlers := append([]sipmod.InviteHandler(nil), svc.inviteHandlers...)
	svc.mu.Unlock()
	for _, handler := range handlers {
		handler(req, tx)
	}
	return tx.getResponse()
}

func newControlPlaneGateway(t *testing.T, cfg config.SIPGatewayConfig) (*Gateway, *mockSIPService, *core.StreamHub) {
	t.Helper()
	svc := &mockSIPService{localAddr: "127.0.0.1:5060", serverID: "test", domain: "test.local"}
	hub := newTestHub()
	gw, err := NewGateway(cfg, svc, hub, core.NewEventBus())
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	t.Cleanup(gw.Close)
	return gw, svc, hub
}

func TestGatewayListsAndDescribesInboundCalls(t *testing.T) {
	gw, svc, _ := newControlPlaneGateway(t, newTestGatewayConfig(t))
	resp := inviteGateway(t, svc, "snapshot-inbound", "camera", []byte(testAudioOffer))
	if resp == nil || resp.StatusCode != 200 {
		t.Fatalf("INVITE status = %v, want 200", resp)
	}

	calls := gw.ListCalls()
	if len(calls) != 1 {
		t.Fatalf("ListCalls length = %d, want 1", len(calls))
	}
	call := calls[0]
	if call.CallID != "snapshot-inbound" || call.Direction != "inbound" || call.StreamKey != "sip/camera" {
		t.Fatalf("unexpected call identity: %+v", call)
	}
	if call.Codec != "PCMA" || call.RTPPort == 0 || call.RTCPPort != call.RTPPort+1 {
		t.Fatalf("unexpected media status: %+v", call)
	}
	if call.RemoteAddress != "127.0.0.1:49998" || call.State != CallStateActive || call.StartedAt.IsZero() {
		t.Fatalf("unexpected call status: %+v", call)
	}

	detail, ok := gw.Call("snapshot-inbound")
	if !ok || detail != call {
		t.Fatalf("Call detail = (%+v, %v), want listed call", detail, ok)
	}
	if _, ok := gw.Call("missing"); ok {
		t.Fatal("Call reported an unknown call-ID")
	}
}

func TestGatewayRejectsDuplicateCallID(t *testing.T) {
	gw, svc, _ := newControlPlaneGateway(t, newTestGatewayConfig(t))
	if resp := inviteGateway(t, svc, "duplicate", "first", []byte(testAudioOffer)); resp == nil || resp.StatusCode != 200 {
		t.Fatalf("first INVITE status = %v, want 200", resp)
	}
	if resp := inviteGateway(t, svc, "duplicate", "second", []byte(testAudioOffer)); resp == nil || resp.StatusCode != 486 {
		t.Fatalf("duplicate INVITE status = %v, want 486", resp)
	}
	if got := gw.ActiveCalls(); got != 1 {
		t.Fatalf("ActiveCalls = %d, want 1", got)
	}
	metrics := gw.Metrics()
	if metrics.DuplicateCallIDs != 1 || metrics.SetupFailures != 1 {
		t.Fatalf("duplicate metrics = %+v", metrics)
	}
}

func TestGatewayReservesCallIDOnceConcurrently(t *testing.T) {
	gw, _, _ := newControlPlaneGateway(t, newTestGatewayConfig(t))
	const attempts = 32
	start := make(chan struct{})
	results := make(chan error, attempts)
	var wg sync.WaitGroup
	wg.Add(attempts)
	for range attempts {
		go func() {
			defer wg.Done()
			<-start
			results <- gw.reserveCall("concurrent-duplicate")
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	reserved := 0
	duplicates := 0
	for err := range results {
		switch {
		case err == nil:
			reserved++
		case errors.Is(err, errDuplicateCall):
			duplicates++
		default:
			t.Fatalf("reserveCall error = %v, want duplicate", err)
		}
	}
	if reserved != 1 || duplicates != attempts-1 {
		t.Fatalf("reservations=%d duplicates=%d, want 1 and %d", reserved, duplicates, attempts-1)
	}
	gw.cancelReservation("concurrent-duplicate")
}

func TestGatewayReportsCodecMismatch(t *testing.T) {
	gw, svc, _ := newControlPlaneGateway(t, newTestGatewayConfig(t))
	offer := []byte("v=0\r\no=- 1 1 IN IP4 127.0.0.1\r\ns=-\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\nm=audio 49998 RTP/AVP 111\r\na=rtpmap:111 opus/48000/2\r\n")
	if resp := inviteGateway(t, svc, "codec-mismatch", "camera", offer); resp == nil || resp.StatusCode != 488 {
		t.Fatalf("INVITE status = %v, want 488", resp)
	}
	metrics := gw.Metrics()
	if metrics.CodecFailures != 1 || metrics.SetupFailures != 1 {
		t.Fatalf("codec metrics = %+v", metrics)
	}
}

func TestGatewayReportsPortExhaustion(t *testing.T) {
	cfg := newTestGatewayConfig(t)
	cfg.RTPPortRange = freeSIPGatewayRTPPortRange(t, 1)
	gw, svc, _ := newControlPlaneGateway(t, cfg)
	if resp := inviteGateway(t, svc, "port-first", "first", []byte(testAudioOffer)); resp == nil || resp.StatusCode != 200 {
		t.Fatalf("first INVITE status = %v, want 200", resp)
	}
	if resp := inviteGateway(t, svc, "port-second", "second", []byte(testAudioOffer)); resp == nil || resp.StatusCode != 503 {
		t.Fatalf("second INVITE status = %v, want 503", resp)
	}
	metrics := gw.Metrics()
	if metrics.PortExhaustions != 1 || metrics.SetupFailures != 1 {
		t.Fatalf("port metrics = %+v", metrics)
	}
}

func TestGatewayRespondsToUnknownBYE(t *testing.T) {
	_, svc, _ := newControlPlaneGateway(t, newTestGatewayConfig(t))
	req := sip.NewRequest(sip.BYE, sip.Uri{User: "missing", Host: "test.local"})
	req.AppendHeader(sip.NewHeader("Call-ID", "missing-call"))
	tx := &mockServerTx{}

	svc.mu.Lock()
	handlers := append([]sipmod.ByeHandler(nil), svc.byeHandlers...)
	svc.mu.Unlock()
	for _, handler := range handlers {
		handler(req, tx)
	}

	resp := tx.getResponse()
	if resp == nil || resp.StatusCode != 481 {
		t.Fatalf("unknown BYE status = %v, want 481", resp)
	}
}

func TestGatewayRTPTimeoutReleasesSessionAndPortOnce(t *testing.T) {
	cfg := newTestGatewayConfig(t)
	cfg.RTPPortRange = freeSIPGatewayRTPPortRange(t, 1)
	gw, svc, hub := newControlPlaneGateway(t, cfg)
	gw.rtpIdleTimeout = 20 * time.Millisecond

	if resp := inviteGateway(t, svc, "network-loss", "first", []byte(testAudioOffer)); resp == nil || resp.StatusCode != 200 {
		t.Fatalf("first INVITE status = %v, want 200", resp)
	}
	deadline := time.Now().Add(time.Second)
	for gw.ActiveCalls() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := gw.ActiveCalls(); got != 0 {
		t.Fatalf("ActiveCalls after RTP timeout = %d, want 0", got)
	}
	stream, ok := hub.Find("sip/first")
	if !ok || stream.Publisher() != nil {
		t.Fatalf("inbound publisher was not removed after RTP timeout")
	}

	// A late BYE and local hangup must not repeat cleanup.
	bye := sip.NewRequest(sip.BYE, sip.Uri{User: "first", Host: "test.local"})
	bye.AppendHeader(sip.NewHeader("Call-ID", "network-loss"))
	byeTx := &mockServerTx{}
	gw.handleBye(bye, byeTx)
	if err := gw.Hangup("network-loss"); err == nil {
		t.Fatal("Hangup succeeded for an already-cleaned call")
	}

	metrics := gw.Metrics()
	if metrics.NetworkFailures != 1 || metrics.CallsEnded != 1 {
		t.Fatalf("terminal metrics = %+v, want one network failure and one ended call", metrics)
	}

	// Successful reuse proves the only RTP pair was returned to the allocator.
	gw.rtpIdleTimeout = time.Second
	if resp := inviteGateway(t, svc, "port-reused", "second", []byte(testAudioOffer)); resp == nil || resp.StatusCode != 200 {
		t.Fatalf("INVITE after timeout status = %v, want 200", resp)
	}
}

type fakeInviteDialog struct {
	done              chan struct{}
	response          *sip.Response
	mu                sync.Mutex
	acks              int
	byes              int
	closes            int
	ackHook           func()
	responseHook      func()
	rejectCanceledACK bool
	byeErr            error
	byeStarted        chan struct{}
	byeRelease        chan struct{}
}

func (d *fakeInviteDialog) Done() <-chan struct{} { return d.done }
func (d *fakeInviteDialog) Response() *sip.Response {
	d.mu.Lock()
	response, hook := d.response, d.responseHook
	d.mu.Unlock()
	if hook != nil {
		hook()
	}
	return response
}
func (d *fakeInviteDialog) Close() {
	d.mu.Lock()
	d.closes++
	d.mu.Unlock()
}
func (d *fakeInviteDialog) SendACK(ctx context.Context) error {
	d.mu.Lock()
	d.acks++
	hook := d.ackHook
	rejectCanceled := d.rejectCanceledACK
	d.mu.Unlock()
	if hook != nil {
		hook()
	}
	if rejectCanceled {
		return ctx.Err()
	}
	return nil
}
func (d *fakeInviteDialog) SendBYE(context.Context) error {
	d.mu.Lock()
	d.byes++
	d.mu.Unlock()
	if d.byeStarted != nil {
		d.byeStarted <- struct{}{}
	}
	if d.byeRelease != nil {
		<-d.byeRelease
	}
	return d.byeErr
}

func TestGatewayDialsListsAndHangsUpOutboundCall(t *testing.T) {
	gw, _, hub := newControlPlaneGateway(t, newTestGatewayConfig(t))
	stream, _ := hub.GetOrCreate("live/outbound")
	publishTestAudio(t, stream, avframe.CodecG711A)
	dialog := &fakeInviteDialog{done: make(chan struct{})}
	close(dialog.done)
	gw.sendInvite = func(_ context.Context, req *sip.Request) (inviteDialog, error) {
		dialog.response = sip.NewResponseFromRequest(req, 200, "OK", []byte(testAudioOffer))
		return dialog, nil
	}

	callID, err := gw.Dial(context.Background(), "alice", "live/outbound")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	call, ok := gw.Call(callID)
	if !ok || call.Direction != "outbound" || call.StreamKey != "live/outbound" || call.State != CallStateActive {
		t.Fatalf("outbound call = (%+v, %v)", call, ok)
	}
	if call.RemoteAddress != "127.0.0.1:49998" || call.Codec != "PCMA" {
		t.Fatalf("outbound media status = %+v", call)
	}
	deadline := time.Now().Add(time.Second)
	for stream.Subscribers()["sipgateway"] != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	stream.WriteFrame(&avframe.AVFrame{
		MediaType: avframe.MediaTypeAudio,
		Codec:     avframe.CodecG711A,
		DTS:       20,
		PTS:       20,
		Payload:   []byte{1, 2, 3, 4},
	})
	deadline = time.Now().Add(time.Second)
	for gw.Metrics().RTPPacketsSent == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	call, ok = gw.Call(callID)
	if !ok || call.RTPPacketsSent != 1 || call.RTPBytesSent != 16 {
		t.Fatalf("outbound RTP counters = (%+v, %v)", call, ok)
	}

	if err := gw.Hangup(callID); err != nil {
		t.Fatalf("Hangup: %v", err)
	}
	if gw.ActiveCalls() != 0 {
		t.Fatalf("ActiveCalls after Hangup = %d, want 0", gw.ActiveCalls())
	}
	dialog.mu.Lock()
	acks, byes := dialog.acks, dialog.byes
	dialog.mu.Unlock()
	if acks != 1 || byes != 1 {
		t.Fatalf("dialog signaling ACK=%d BYE=%d, want 1 each", acks, byes)
	}
	metrics := gw.Metrics()
	if metrics.CallsStarted != 1 || metrics.CallsEnded != 1 || metrics.ActiveCalls != 0 {
		t.Fatalf("call metrics = %+v", metrics)
	}
	if metrics.RTPPacketsSent != 1 || metrics.RTPBytesSent == 0 {
		t.Fatalf("outbound RTP metrics = %+v", metrics)
	}
}

func TestGatewayCountsInboundRTPPacketsAndBytes(t *testing.T) {
	cfg := newTestGatewayConfig(t)
	rtpPort := evenSIPLabPort(t)
	cfg.RTPPortRange = []int{rtpPort, rtpPort + 1}
	gw, svc, _ := newControlPlaneGateway(t, cfg)
	if resp := inviteGateway(t, svc, "rtp-counters", "counter-stream", []byte(testAudioOffer)); resp == nil || resp.StatusCode != 200 {
		t.Fatalf("INVITE status = %v, want 200", resp)
	}
	call, ok := gw.Call("rtp-counters")
	if !ok {
		t.Fatal("active call not found")
	}
	conn, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: call.RTPPort})
	if err != nil {
		t.Fatalf("dial RTP socket: %v", err)
	}
	defer conn.Close()
	packet := &pionrtp.Packet{
		Header:  pionrtp.Header{Version: 2, PayloadType: 8, SequenceNumber: 1, Timestamp: 160, SSRC: 1},
		Payload: []byte{1, 2, 3, 4},
	}
	data, err := packet.Marshal()
	if err != nil {
		t.Fatalf("marshal RTP: %v", err)
	}
	if _, err := conn.Write(data); err != nil {
		t.Fatalf("write RTP: %v", err)
	}

	deadline := time.Now().Add(time.Second)
	for gw.Metrics().RTPPacketsRecv == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	metrics := gw.Metrics()
	if metrics.RTPPacketsRecv != 1 || metrics.RTPBytesRecv != uint64(len(data)) {
		t.Fatalf("inbound RTP metrics = %+v, packet bytes = %d", metrics, len(data))
	}
	call, ok = gw.Call("rtp-counters")
	if !ok || call.RTPPacketsRecv != 1 || call.RTPBytesRecv != uint64(len(data)) || call.LastRTPAt.IsZero() {
		t.Fatalf("inbound call RTP counters = (%+v, %v)", call, ok)
	}
}

func TestSIPGatewayStalePublisherCallbackCannotWriteReplacementGeneration(t *testing.T) {
	cfg := newTestGatewayConfig(t)
	rtpPort := evenSIPLabPort(t)
	cfg.RTPPortRange = []int{rtpPort, rtpPort + 1}
	gw, svc, hub := newControlPlaneGateway(t, cfg)
	if resp := inviteGateway(t, svc, "stale-writer", "stale-writer", []byte(testAudioOffer)); resp == nil || resp.StatusCode != 200 {
		t.Fatalf("INVITE status = %v, want 200", resp)
	}
	call, ok := gw.Call("stale-writer")
	if !ok {
		t.Fatal("active inbound call not found")
	}
	stream, ok := hub.Find("sip/stale-writer")
	if !ok {
		t.Fatal("inbound stream not found")
	}
	conn, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: call.RTPPort})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	sendPacket := func(sequence uint16) {
		t.Helper()
		packet := &pionrtp.Packet{
			Header:  pionrtp.Header{Version: 2, PayloadType: 8, SequenceNumber: sequence, Timestamp: uint32(sequence) * 160, SSRC: 1},
			Payload: []byte{byte(sequence), 2, 3, 4},
		}
		data, marshalErr := packet.Marshal()
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if _, writeErr := conn.Write(data); writeErr != nil {
			t.Fatal(writeErr)
		}
	}
	waitForReceived := func(want uint64) {
		t.Helper()
		deadline := time.Now().Add(time.Second)
		for gw.Metrics().RTPPacketsRecv < want && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if got := gw.Metrics().RTPPacketsRecv; got < want {
			t.Fatalf("received RTP packets = %d, want at least %d", got, want)
		}
	}

	sendPacket(1)
	waitForReceived(1)
	deadline := time.Now().Add(time.Second)
	for stream.RingBuffer().WriteCursor() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := stream.RingBuffer().WriteCursor(); got != 1 {
		t.Fatalf("active SIP callback cursor = %d, want 1", got)
	}
	oldPublisher := stream.Publisher()
	if !stream.RemovePublisherIf(oldPublisher) {
		t.Fatal("old SIP publisher was not removed")
	}
	replacement := &sipPublisher{id: "sip-replacement", info: &avframe.MediaInfo{AudioCodec: avframe.CodecG711A}}
	if err := stream.SetPublisher(replacement); err != nil {
		t.Fatal(err)
	}
	startCursor := stream.StartupSnapshot().GenerationStartCursor

	sendPacket(2)
	waitForReceived(2)
	time.Sleep(10 * time.Millisecond)
	if got := stream.RingBuffer().WriteCursor(); got != startCursor {
		t.Fatalf("stale SIP callback advanced replacement cursor to %d, want %d", got, startCursor)
	}
	deadline = time.Now().Add(time.Second)
	for gw.ActiveCalls() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := gw.ActiveCalls(); got != 0 {
		t.Fatalf("stale SIP publisher left %d active calls", got)
	}
}

func TestGatewayCleansUpWhenRTPNetworkReadFails(t *testing.T) {
	gw, svc, _ := newControlPlaneGateway(t, newTestGatewayConfig(t))
	if resp := inviteGateway(t, svc, "read-failure", "live-test", []byte(testAudioOffer)); resp == nil || resp.StatusCode != 200 {
		t.Fatalf("INVITE status = %v, want 200", resp)
	}
	gw.mu.RLock()
	cs := gw.sessions["read-failure"]
	gw.mu.RUnlock()
	if cs == nil {
		t.Fatal("active session not found")
	}
	if err := cs.conn.Close(); err != nil {
		t.Fatalf("close RTP socket: %v", err)
	}

	deadline := time.Now().Add(time.Second)
	for gw.ActiveCalls() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	_ = cs.conn.Close()

	if gw.ActiveCalls() != 0 {
		t.Fatalf("ActiveCalls = %d, want 0", gw.ActiveCalls())
	}
	failed := cs.snapshot()
	if failed.State != CallStateNetworkLost || failed.LastError == "" {
		t.Fatalf("network failure state = %+v", failed)
	}
	metrics := gw.Metrics()
	if metrics.NetworkFailures != 1 || metrics.CallsEnded != 1 {
		t.Fatalf("network metrics = %+v", metrics)
	}
}

func TestCallSessionCannotStartAfterTermination(t *testing.T) {
	hub := newTestHub()
	stream, _ := hub.GetOrCreate("sip/closed-before-start")
	cs := newCallSession("closed-before-start", stream.Key(), negotiatedCodec{
		Codec:        8,
		PT:           8,
		ClockRate:    8000,
		EncodingName: "PCMA",
	}, "inbound", 44200, 44201)
	cs.Close()

	if err := cs.startInbound(stream, "127.0.0.1", 49998); err == nil {
		t.Fatal("startInbound succeeded after session termination")
	}
	if publisher := stream.Publisher(); publisher != nil {
		t.Fatalf("terminated session attached publisher %q", publisher.ID())
	}
}

func TestConcurrentHangupSignalsAndCleansOutboundCallOnce(t *testing.T) {
	gw, _, hub := newControlPlaneGateway(t, newTestGatewayConfig(t))
	stream, _ := hub.GetOrCreate("live/concurrent-hangup")
	publishTestAudio(t, stream, avframe.CodecG711A)
	dialog := &fakeInviteDialog{
		done:       make(chan struct{}),
		byeStarted: make(chan struct{}, 2),
		byeRelease: make(chan struct{}),
	}
	close(dialog.done)
	gw.sendInvite = func(_ context.Context, req *sip.Request) (inviteDialog, error) {
		dialog.response = sip.NewResponseFromRequest(req, 200, "OK", []byte(testAudioOffer))
		return dialog, nil
	}
	callID, err := gw.Dial(context.Background(), "alice", "live/concurrent-hangup")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	for range 2 {
		go func() {
			defer wg.Done()
			_ = gw.Hangup(callID)
		}()
	}
	select {
	case <-dialog.byeStarted:
	case <-time.After(time.Second):
		t.Fatal("first BYE was not sent")
	}
	time.Sleep(20 * time.Millisecond)
	close(dialog.byeRelease)
	wg.Wait()

	dialog.mu.Lock()
	byes := dialog.byes
	dialog.mu.Unlock()
	if byes != 1 {
		t.Fatalf("concurrent Hangup sent %d BYEs, want 1", byes)
	}
	metrics := gw.Metrics()
	if metrics.CallsEnded != 1 {
		t.Fatalf("CallsEnded = %d, want 1", metrics.CallsEnded)
	}
}

func TestModuleExposesGatewayProviderMethodsWhenDisabled(t *testing.T) {
	m := NewModule(&mockSIPService{})
	if calls := m.ListCalls(); len(calls) != 0 {
		t.Fatalf("disabled ListCalls = %+v, want empty", calls)
	}
	if _, ok := m.Call("missing"); ok {
		t.Fatal("disabled module returned call detail")
	}
	if _, err := m.Dial(context.Background(), "alice", "live/test"); !errors.Is(err, ErrGatewayDisabled) {
		t.Fatalf("disabled Dial error = %v, want ErrGatewayDisabled", err)
	}
	if err := m.Hangup("missing"); !errors.Is(err, ErrGatewayDisabled) {
		t.Fatalf("disabled Hangup error = %v, want ErrGatewayDisabled", err)
	}
}

func TestGatewayControlPlaneErrorsAreClassifiable(t *testing.T) {
	gw, _, _ := newControlPlaneGateway(t, newTestGatewayConfig(t))
	if err := gw.Hangup("missing"); !errors.Is(err, ErrCallNotFound) {
		t.Fatalf("Hangup error = %v, want ErrCallNotFound", err)
	}
	if _, err := gw.Dial(context.Background(), "alice", "missing/stream"); !errors.Is(err, ErrStreamNotFound) {
		t.Fatalf("Dial error = %v, want ErrStreamNotFound", err)
	}
}

func TestModuleExposesBoundedPrometheusMetrics(t *testing.T) {
	gw, svc, _ := newControlPlaneGateway(t, newTestGatewayConfig(t))
	if resp := inviteGateway(t, svc, "metrics-call-id", "metrics-stream", []byte(testAudioOffer)); resp == nil || resp.StatusCode != 200 {
		t.Fatalf("INVITE status = %v, want 200", resp)
	}
	m := &Module{gw: gw}
	collectors := m.PrometheusCollectors()
	if len(collectors) != 1 {
		t.Fatalf("PrometheusCollectors length = %d, want 1", len(collectors))
	}
	registry := prometheus.NewRegistry()
	registry.MustRegister(collectors[0])

	expected := `
# HELP liveforge_sipgateway_active_calls Number of active SIP gateway calls.
# TYPE liveforge_sipgateway_active_calls gauge
liveforge_sipgateway_active_calls{direction="inbound"} 1
liveforge_sipgateway_active_calls{direction="outbound"} 0
# HELP liveforge_sipgateway_calls_started_total Total SIP gateway calls established.
# TYPE liveforge_sipgateway_calls_started_total counter
liveforge_sipgateway_calls_started_total 1
# HELP liveforge_sipgateway_setup_failures_total Total SIP gateway call setup failures.
# TYPE liveforge_sipgateway_setup_failures_total counter
liveforge_sipgateway_setup_failures_total 0
`
	if err := testutil.GatherAndCompare(registry, strings.NewReader(expected),
		"liveforge_sipgateway_active_calls",
		"liveforge_sipgateway_calls_started_total",
		"liveforge_sipgateway_setup_failures_total",
	); err != nil {
		t.Fatalf("gather SIP gateway metrics: %v", err)
	}
}
