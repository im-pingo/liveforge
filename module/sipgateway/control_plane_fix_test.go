package sipgateway

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/sdp"
	"github.com/pion/rtcp"
	pionrtp "github.com/pion/rtp/v2"
)

type gatewayTestPublisher struct {
	id   string
	info *avframe.MediaInfo
}

func (p *gatewayTestPublisher) ID() string                    { return p.id }
func (p *gatewayTestPublisher) MediaInfo() *avframe.MediaInfo { return p.info }
func (p *gatewayTestPublisher) Close() error                  { return nil }

func publishTestAudio(t *testing.T, stream *core.Stream, codec avframe.CodecType) {
	t.Helper()
	if err := stream.SetPublisher(&gatewayTestPublisher{
		id:   "test-publisher",
		info: &avframe.MediaInfo{AudioCodec: codec, SampleRate: 8000, Channels: 1},
	}); err != nil {
		t.Fatalf("SetPublisher: %v", err)
	}
}

func TestGatewayRejectsOutboundSourceCodecMismatchBeforeInvite(t *testing.T) {
	gw, _, hub := newControlPlaneGateway(t, newTestGatewayConfig())
	stream, _ := hub.GetOrCreate("live/aac")
	publishTestAudio(t, stream, avframe.CodecAAC)
	invites := 0
	gw.sendInvite = func(context.Context, *sip.Request) (inviteDialog, error) {
		invites++
		return nil, errors.New("unexpected INVITE")
	}

	if _, err := gw.Dial(context.Background(), "alice", stream.Key()); !errors.Is(err, ErrCodecMismatch) {
		t.Fatalf("Dial error = %v, want ErrCodecMismatch", err)
	}
	if invites != 0 {
		t.Fatalf("INVITE count = %d, want 0", invites)
	}
	metrics := gw.Metrics()
	if metrics.CodecFailures != 1 || metrics.SetupFailures != 1 {
		t.Fatalf("codec metrics = %+v, want one codec and setup failure", metrics)
	}
}

func TestGatewayAdvertisesOnlyImplementedSourceCodec(t *testing.T) {
	cfg := newTestGatewayConfig()
	cfg.Codecs = []string{"G729", "speex", "MPEG4-GENERIC", "PCMA"}
	gw, _, hub := newControlPlaneGateway(t, cfg)
	stream, _ := hub.GetOrCreate("live/pcma")
	publishTestAudio(t, stream, avframe.CodecG711A)
	dialog := &fakeInviteDialog{done: make(chan struct{})}
	close(dialog.done)
	gw.sendInvite = func(_ context.Context, req *sip.Request) (inviteDialog, error) {
		offer, err := sdp.Parse(req.Body())
		if err != nil {
			t.Fatalf("Parse offer: %v", err)
		}
		if len(offer.Media) != 1 || len(offer.Media[0].Formats) != 1 || offer.Media[0].Formats[0] != 8 {
			t.Fatalf("offered formats = %+v, want only PCMA payload 8", offer.Media)
		}
		dialog.response = sip.NewResponseFromRequest(req, 200, "OK", []byte(testAudioOffer))
		return dialog, nil
	}

	callID, err := gw.Dial(context.Background(), "alice", stream.Key())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if err := gw.Hangup(callID); err != nil {
		t.Fatalf("Hangup: %v", err)
	}
}

func TestGatewayRejectsInboundCodecWithoutPacketizerSupport(t *testing.T) {
	cfg := newTestGatewayConfig()
	cfg.Codecs = []string{"G729"}
	gw, svc, _ := newControlPlaneGateway(t, cfg)
	offer := []byte("v=0\r\no=- 1 1 IN IP4 127.0.0.1\r\ns=-\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\nm=audio 49998 RTP/AVP 18\r\na=rtpmap:18 G729/8000\r\n")

	if resp := inviteGateway(t, svc, "unsupported-g729", "camera", offer); resp == nil || resp.StatusCode != 488 {
		t.Fatalf("INVITE status = %v, want 488", resp)
	}
	if got := gw.Metrics().CodecFailures; got != 1 {
		t.Fatalf("CodecFailures = %d, want 1", got)
	}
}

func TestGatewayRoutesSIPURIAndBareExtension(t *testing.T) {
	tests := []struct {
		name     string
		target   string
		wantUser string
		wantHost string
		wantPort int
	}{
		{name: "full URI", target: "sip:alice@pbx.example.com", wantUser: "alice", wantHost: "pbx.example.com"},
		{name: "full URI with port", target: "sip:bob@192.0.2.40:5072", wantUser: "bob", wantHost: "192.0.2.40", wantPort: 5072},
		{name: "bare extension", target: "1001", wantUser: "1001", wantHost: "test.local"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gw, _, hub := newControlPlaneGateway(t, newTestGatewayConfig())
			stream, _ := hub.GetOrCreate("live/uri")
			publishTestAudio(t, stream, avframe.CodecG711A)
			dialog := &fakeInviteDialog{done: make(chan struct{})}
			close(dialog.done)
			gw.sendInvite = func(_ context.Context, req *sip.Request) (inviteDialog, error) {
				if req.Recipient.User != tt.wantUser || req.Recipient.Host != tt.wantHost || req.Recipient.Port != tt.wantPort {
					t.Fatalf("request URI = %s, want sip:%s@%s port %d", req.Recipient.String(), tt.wantUser, tt.wantHost, tt.wantPort)
				}
				dialog.response = sip.NewResponseFromRequest(req, 200, "OK", []byte(testAudioOffer))
				return dialog, nil
			}

			callID, err := gw.Dial(context.Background(), tt.target, stream.Key())
			if err != nil {
				t.Fatalf("Dial: %v", err)
			}
			if err := gw.Hangup(callID); err != nil {
				t.Fatalf("Hangup: %v", err)
			}
		})
	}
}

func TestGatewayRejectsMalformedSIPURI(t *testing.T) {
	gw, _, hub := newControlPlaneGateway(t, newTestGatewayConfig())
	stream, _ := hub.GetOrCreate("live/uri-invalid")
	publishTestAudio(t, stream, avframe.CodecG711A)

	for _, target := range []string{"alice@example.com", "sip:alice", "sip:@example.com", "sip:alice@example.com:70000", "http:alice@example.com"} {
		t.Run(target, func(t *testing.T) {
			if _, err := gw.Dial(context.Background(), target, stream.Key()); !errors.Is(err, ErrInvalidTargetURI) {
				t.Fatalf("Dial(%q) error = %v, want ErrInvalidTargetURI", target, err)
			}
		})
	}
}

func TestGatewayAcknowledgesAndTearsDownEveryFailed2xxDialog(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{name: "malformed SDP on non-200 2xx", status: 202, body: "not-sdp"},
		{name: "answer has no audio", status: 200, body: "v=0\r\no=- 1 1 IN IP4 127.0.0.1\r\ns=-\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\nm=video 49998 RTP/AVP 96\r\n"},
		{name: "answer selects wrong codec", status: 200, body: "v=0\r\no=- 1 1 IN IP4 127.0.0.1\r\ns=-\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\nm=audio 49998 RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\n"},
		{name: "answer selects wrong clock rate", status: 200, body: "v=0\r\no=- 1 1 IN IP4 127.0.0.1\r\ns=-\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\nm=audio 49998 RTP/AVP 8\r\na=rtpmap:8 PCMA/16000\r\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gw, _, hub := newControlPlaneGateway(t, newTestGatewayConfig())
			stream, _ := hub.GetOrCreate("live/post-2xx")
			publishTestAudio(t, stream, avframe.CodecG711A)
			dialog := &fakeInviteDialog{done: make(chan struct{})}
			close(dialog.done)
			gw.sendInvite = func(_ context.Context, req *sip.Request) (inviteDialog, error) {
				dialog.response = sip.NewResponseFromRequest(req, tt.status, "Accepted", []byte(tt.body))
				return dialog, nil
			}

			if _, err := gw.Dial(context.Background(), "alice", stream.Key()); err == nil {
				t.Fatal("Dial succeeded for invalid 2xx answer")
			}
			dialog.mu.Lock()
			acks, byes := dialog.acks, dialog.byes
			dialog.mu.Unlock()
			if acks != 1 || byes != 1 {
				t.Fatalf("dialog signaling ACK=%d BYE=%d, want 1 each", acks, byes)
			}
		})
	}
}

func TestGatewayAcknowledges2xxAfterCallerContextCancellation(t *testing.T) {
	gw, _, hub := newControlPlaneGateway(t, newTestGatewayConfig())
	stream, _ := hub.GetOrCreate("live/ack-context")
	publishTestAudio(t, stream, avframe.CodecG711A)
	ctx, cancel := context.WithCancel(context.Background())
	dialog := &fakeInviteDialog{
		done:              make(chan struct{}),
		responseHook:      cancel,
		rejectCanceledACK: true,
	}
	close(dialog.done)
	gw.sendInvite = func(_ context.Context, req *sip.Request) (inviteDialog, error) {
		dialog.response = sip.NewResponseFromRequest(req, 200, "OK", []byte(testAudioOffer))
		return dialog, nil
	}

	callID, err := gw.Dial(ctx, "alice", stream.Key())
	if err != nil {
		t.Fatalf("Dial after 2xx caller cancellation: %v", err)
	}
	if err := gw.Hangup(callID); err != nil {
		t.Fatalf("Hangup: %v", err)
	}
	dialog.mu.Lock()
	acks, byes := dialog.acks, dialog.byes
	dialog.mu.Unlock()
	if acks != 1 || byes != 1 {
		t.Fatalf("dialog signaling ACK=%d BYE=%d, want 1 each", acks, byes)
	}
}

func TestGatewayTearsDownDialogWhenActivationFailsAfterACK(t *testing.T) {
	gw, _, hub := newControlPlaneGateway(t, newTestGatewayConfig())
	stream, _ := hub.GetOrCreate("live/activation-failure")
	publishTestAudio(t, stream, avframe.CodecG711A)
	dialog := &fakeInviteDialog{done: make(chan struct{})}
	close(dialog.done)
	dialog.ackHook = gw.Close
	gw.sendInvite = func(_ context.Context, req *sip.Request) (inviteDialog, error) {
		dialog.response = sip.NewResponseFromRequest(req, 200, "OK", []byte(testAudioOffer))
		return dialog, nil
	}

	if _, err := gw.Dial(context.Background(), "alice", stream.Key()); !errors.Is(err, ErrGatewayClosed) {
		t.Fatalf("Dial error = %v, want ErrGatewayClosed", err)
	}
	dialog.mu.Lock()
	acks, byes := dialog.acks, dialog.byes
	dialog.mu.Unlock()
	if acks != 1 || byes != 1 {
		t.Fatalf("dialog signaling ACK=%d BYE=%d, want 1 each", acks, byes)
	}
}

func TestGatewayTearsDownDialogWhenMediaStartFailsAfterACK(t *testing.T) {
	gw, _, hub := newControlPlaneGateway(t, newTestGatewayConfig())
	stream, _ := hub.GetOrCreate("live/start-failure")
	publishTestAudio(t, stream, avframe.CodecG711A)
	dialog := &fakeInviteDialog{done: make(chan struct{})}
	close(dialog.done)
	invalidRemote := "v=0\r\no=- 1 1 IN IP4 invalid.example\r\ns=-\r\nc=IN IP4 invalid.example\r\nt=0 0\r\nm=audio 49998 RTP/AVP 8\r\na=rtpmap:8 PCMA/8000\r\n"
	gw.sendInvite = func(_ context.Context, req *sip.Request) (inviteDialog, error) {
		dialog.response = sip.NewResponseFromRequest(req, 200, "OK", []byte(invalidRemote))
		return dialog, nil
	}

	if _, err := gw.Dial(context.Background(), "alice", stream.Key()); err == nil {
		t.Fatal("Dial succeeded with invalid remote RTP address")
	}
	dialog.mu.Lock()
	acks, byes := dialog.acks, dialog.byes
	dialog.mu.Unlock()
	if acks != 1 || byes != 1 {
		t.Fatalf("dialog signaling ACK=%d BYE=%d, want 1 each", acks, byes)
	}
}

func TestGatewayAndSessionCloseRaceTearsDownOutboundDialogOnce(t *testing.T) {
	gw, _, hub := newControlPlaneGateway(t, newTestGatewayConfig())
	stream, _ := hub.GetOrCreate("live/close-race")
	publishTestAudio(t, stream, avframe.CodecG711A)
	dialog := &fakeInviteDialog{done: make(chan struct{})}
	close(dialog.done)
	gw.sendInvite = func(_ context.Context, req *sip.Request) (inviteDialog, error) {
		dialog.response = sip.NewResponseFromRequest(req, 200, "OK", []byte(testAudioOffer))
		return dialog, nil
	}
	callID, err := gw.Dial(context.Background(), "alice", stream.Key())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	gw.mu.RLock()
	session := gw.sessions[callID]
	gw.mu.RUnlock()
	if session == nil {
		t.Fatal("active outbound session not found")
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); <-start; gw.Close() }()
	go func() { defer wg.Done(); <-start; session.Close() }()
	close(start)
	wg.Wait()

	dialog.mu.Lock()
	byes := dialog.byes
	dialog.mu.Unlock()
	if byes != 1 {
		t.Fatalf("close race sent %d BYEs, want 1", byes)
	}
}

func TestGatewayRemoteBYEEndsOutboundDialogWithoutSendingBYE(t *testing.T) {
	gw, _, hub := newControlPlaneGateway(t, newTestGatewayConfig())
	stream, _ := hub.GetOrCreate("live/remote-bye")
	publishTestAudio(t, stream, avframe.CodecG711A)
	dialog := &fakeInviteDialog{done: make(chan struct{})}
	close(dialog.done)
	gw.sendInvite = func(_ context.Context, req *sip.Request) (inviteDialog, error) {
		dialog.response = sip.NewResponseFromRequest(req, 200, "OK", []byte(testAudioOffer))
		return dialog, nil
	}
	callID, err := gw.Dial(context.Background(), "alice", stream.Key())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	bye := sip.NewRequest(sip.BYE, sip.Uri{User: "test", Host: "test.local"})
	bye.AppendHeader(sip.NewHeader("Call-ID", callID))
	tx := &mockServerTx{}

	gw.handleBye(bye, tx)

	if resp := tx.getResponse(); resp == nil || resp.StatusCode != 200 {
		t.Fatalf("remote BYE response = %v, want 200", resp)
	}
	dialog.mu.Lock()
	byes := dialog.byes
	dialog.mu.Unlock()
	if byes != 0 {
		t.Fatalf("remote BYE triggered %d outbound BYEs, want 0", byes)
	}
	call, ok := gw.Call(callID)
	if !ok || call.State != CallStateEnded {
		t.Fatalf("terminal call = (%+v, %v), want ended", call, ok)
	}
}

func TestGatewayOutboundRTCPReverseLivenessBecomesNetworkLost(t *testing.T) {
	gw, _, hub := newControlPlaneGateway(t, newTestGatewayConfig())
	gw.rtpIdleTimeout = 40 * time.Millisecond
	stream, _ := hub.GetOrCreate("live/rtcp-liveness")
	publishTestAudio(t, stream, avframe.CodecG711A)
	dialog := &fakeInviteDialog{done: make(chan struct{})}
	close(dialog.done)
	gw.sendInvite = func(_ context.Context, req *sip.Request) (inviteDialog, error) {
		dialog.response = sip.NewResponseFromRequest(req, 200, "OK", []byte(testAudioOffer))
		return dialog, nil
	}
	callID, err := gw.Dial(context.Background(), "alice", stream.Key())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	call, ok := gw.Call(callID)
	if !ok {
		t.Fatal("active outbound call not found")
	}
	rtcpConn, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: call.RTCPPort})
	if err != nil {
		t.Fatalf("dial RTCP socket: %v", err)
	}
	defer rtcpConn.Close()
	report, err := (&rtcp.ReceiverReport{SSRC: 42}).Marshal()
	if err != nil {
		t.Fatalf("marshal RTCP receiver report: %v", err)
	}

	keepAliveUntil := time.Now().Add(4 * gw.rtpIdleTimeout)
	for time.Now().Before(keepAliveUntil) {
		if _, err := rtcpConn.Write(report); err != nil {
			t.Fatalf("write RTCP receiver report: %v", err)
		}
		time.Sleep(gw.rtpIdleTimeout / 4)
		if gw.ActiveCalls() != 1 {
			t.Fatal("outbound call ended while valid reverse RTCP was arriving")
		}
	}

	deadline := time.Now().Add(500 * time.Millisecond)
	for gw.ActiveCalls() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := gw.ActiveCalls(); got != 0 {
		t.Fatalf("ActiveCalls after RTCP silence = %d, want 0", got)
	}
	terminal, ok := gw.Call(callID)
	if !ok || terminal.State != CallStateNetworkLost || !strings.Contains(terminal.LastError, "RTCP liveness timeout") {
		t.Fatalf("terminal call = (%+v, %v), want observable RTCP network loss", terminal, ok)
	}
	metrics := gw.Metrics()
	if metrics.NetworkFailures != 1 || metrics.CallsEnded != 1 {
		t.Fatalf("network metrics = %+v, want one failure and one ended call", metrics)
	}
}

func TestGatewayKeepsBoundedSortedTerminalHistoryOutsideCapacity(t *testing.T) {
	cfg := newTestGatewayConfig()
	cfg.MaxCalls = 1
	gw, _, _ := newControlPlaneGateway(t, cfg)

	for i := 0; i < 105; i++ {
		callID := fmt.Sprintf("history-%03d", i)
		rtpPort, rtcpPort, err := gw.portAlloc.AllocatePair()
		if err != nil {
			t.Fatalf("AllocatePair %d: %v", i, err)
		}
		if err := gw.reserveCall(callID); err != nil {
			t.Fatalf("reserveCall %d: %v", i, err)
		}
		session := newCallSession(callID, "live/history", negotiatedCodec{
			Codec: avframe.CodecG711A, PT: 8, ClockRate: 8000, EncodingName: "PCMA",
		}, "outbound", rtpPort, rtcpPort)
		gw.configureSession(session)
		if err := gw.activateReservedCall(session); err != nil {
			t.Fatalf("activateReservedCall %d: %v", i, err)
		}
		session.established.Store(true)
		if !gw.finishSession(session, CallStateEnded, nil) {
			t.Fatalf("finishSession %d returned false", i)
		}
	}

	calls := gw.ListCalls()
	if len(calls) != 100 {
		t.Fatalf("terminal history length = %d, want 100", len(calls))
	}
	if calls[0].CallID != "history-005" || calls[len(calls)-1].CallID != "history-104" {
		t.Fatalf("terminal history bounds = %q..%q, want history-005..history-104", calls[0].CallID, calls[len(calls)-1].CallID)
	}
	if _, ok := gw.Call("history-004"); ok {
		t.Fatal("evicted terminal call remained addressable")
	}
	if call, ok := gw.Call("history-104"); !ok || call.State != CallStateEnded {
		t.Fatalf("latest terminal call = (%+v, %v), want ended snapshot", call, ok)
	}
	if err := gw.reserveCall("capacity-after-history"); err != nil {
		t.Fatalf("terminal history consumed active capacity: %v", err)
	}
	gw.cancelReservation("capacity-after-history")
}

func TestGatewayRedactsTerminalCallErrors(t *testing.T) {
	gw, _, _ := newControlPlaneGateway(t, newTestGatewayConfig())
	rtpPort, rtcpPort, err := gw.portAlloc.AllocatePair()
	if err != nil {
		t.Fatalf("AllocatePair: %v", err)
	}
	if err := gw.reserveCall("redacted-error"); err != nil {
		t.Fatalf("reserveCall: %v", err)
	}
	session := newCallSession("redacted-error", "live/redacted", negotiatedCodec{
		Codec: avframe.CodecG711A, PT: 8, ClockRate: 8000, EncodingName: "PCMA",
	}, "outbound", rtpPort, rtcpPort)
	gw.configureSession(session)
	if err := gw.activateReservedCall(session); err != nil {
		t.Fatalf("activateReservedCall: %v", err)
	}
	secretErr := errors.New("dialog sip:alice:topsecret@pbx.example.com\r\nAuthorization: Bearer secret-token")
	if !gw.finishSession(session, CallStateNetworkLost, secretErr) {
		t.Fatal("finishSession returned false")
	}

	call, ok := gw.Call("redacted-error")
	if !ok || call.LastError == "" {
		t.Fatalf("terminal call = (%+v, %v), want last_error", call, ok)
	}
	if strings.Contains(call.LastError, "topsecret") || strings.Contains(call.LastError, "secret-token") || strings.ContainsAny(call.LastError, "\r\n") {
		t.Fatalf("terminal last_error exposed sensitive or multiline data: %q", call.LastError)
	}
}

func TestCallSessionRejectsUnknownPacketizerAndDepacketizerCodec(t *testing.T) {
	session := newCallSession("unsupported-codec", "live/unsupported", negotiatedCodec{
		Codec: avframe.CodecG729, PT: 18, ClockRate: 8000, EncodingName: "G729",
	}, "outbound", 40000, 40001)

	frame := &avframe.AVFrame{MediaType: avframe.MediaTypeAudio, Codec: avframe.CodecG729, Payload: []byte{1, 2, 3}}
	if _, err := session.newPacketizer().packetize(frame); !errors.Is(err, ErrCodecMismatch) {
		t.Fatalf("unknown packetizer error = %v, want ErrCodecMismatch", err)
	}
	packet := &pionrtp.Packet{Header: pionrtp.Header{Version: 2, PayloadType: 18}, Payload: []byte{1, 2, 3}}
	if _, err := session.newDepacketizer().depacketize(packet); !errors.Is(err, ErrCodecMismatch) {
		t.Fatalf("unknown depacketizer error = %v, want ErrCodecMismatch", err)
	}
}
