package sipgateway

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	sipmod "github.com/im-pingo/liveforge/module/sip"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/sdp"
	"github.com/pion/rtcp"
	pionrtp "github.com/pion/rtp/v2"
)

type blockingTerminalDialog struct {
	entered chan struct{}
	release chan struct{}
}

func (d *blockingTerminalDialog) Done() <-chan struct{} {
	done := make(chan struct{})
	return done
}
func (d *blockingTerminalDialog) Response() *sip.Response       { return nil }
func (d *blockingTerminalDialog) SendACK(context.Context) error { return nil }
func (d *blockingTerminalDialog) SendBYE(context.Context) error {
	close(d.entered)
	<-d.release
	return nil
}
func (d *blockingTerminalDialog) Close() {}

func TestTerminalMetricsPublishAtomicallyWithSessionRemoval(t *testing.T) {
	gw, _, _ := newControlPlaneGateway(t, newTestGatewayConfig(t))
	rtpPort, rtcpPort, err := gw.portAlloc.AllocatePair()
	if err != nil {
		t.Fatal(err)
	}
	if err := gw.reserveCall("atomic-terminal"); err != nil {
		t.Fatal(err)
	}
	session := newCallSession("atomic-terminal", "sip/atomic", negotiatedCodec{
		Codec: avframe.CodecG711A, PT: 8, ClockRate: 8000, EncodingName: "PCMA",
	}, "outbound", rtpPort, rtcpPort)
	dialog := &blockingTerminalDialog{entered: make(chan struct{}), release: make(chan struct{})}
	session.dialog = newDialogTeardown(dialog)
	gw.configureSession(session)
	if err := gw.activateReservedCall(session); err != nil {
		t.Fatal(err)
	}
	session.established.Store(true)
	finishDone := make(chan bool, 1)
	go func() {
		finishDone <- gw.finishSession(session, CallStateNetworkLost, errors.New("network lost"))
	}()
	select {
	case <-dialog.entered:
	case <-time.After(time.Second):
		t.Fatal("terminal cleanup did not reach dialog teardown")
	}

	window := gw.Metrics()
	close(dialog.release)
	if !<-finishDone {
		t.Fatal("finishSession did not own terminal transition")
	}
	if window.ActiveCalls == 0 && window.NetworkFailures == 1 && window.CallsEnded == 0 {
		t.Fatalf("observed partial terminal metrics: %+v", window)
	}
}

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

type offeredFinalClientTx struct {
	responses chan *sip.Response
	done      chan struct{}
	once      sync.Once
}

func newOfferedFinalClientTx() *offeredFinalClientTx {
	return &offeredFinalClientTx{
		responses: make(chan *sip.Response),
		done:      make(chan struct{}),
	}
}

func (tx *offeredFinalClientTx) Responses() <-chan *sip.Response { return tx.responses }
func (tx *offeredFinalClientTx) Done() <-chan struct{}           { return tx.done }
func (tx *offeredFinalClientTx) Err() error                      { return nil }
func (tx *offeredFinalClientTx) Terminate()                      { tx.once.Do(func() { close(tx.done) }) }
func (tx *offeredFinalClientTx) OnTerminate(sip.FnTxTerminate) bool {
	return true
}
func (tx *offeredFinalClientTx) OnRetransmission(sip.FnTxResponse) bool {
	return false
}

type gatewayRaceRequester struct {
	cancel     context.CancelFunc
	inviteTx   *offeredFinalClientTx
	senderDone chan struct{}
	ackRequest func(context.Context, *sip.Request) error

	mu      sync.Mutex
	invites int
	acks    int
	byes    int
}

func (r *gatewayRaceRequester) Request(ctx context.Context, req *sip.Request) (sip.ClientTransaction, error) {
	if req.Method == sip.ACK && r.ackRequest != nil {
		err := r.ackRequest(ctx, req)
		tx := newOfferedFinalClientTx()
		tx.Terminate()
		return tx, err
	}

	r.record(req.Method)

	if req.Method != sip.INVITE {
		tx := newOfferedFinalClientTx()
		tx.Terminate()
		return tx, nil
	}

	offerStarted := make(chan struct{})
	go func() {
		close(offerStarted)
		r.inviteTx.responses <- sip.NewResponseFromRequest(req, 200, "OK", []byte(testAudioOffer))
		close(r.senderDone)
	}()
	<-offerStarted
	runtime.Gosched()
	r.cancel()
	return r.inviteTx, nil
}

func (r *gatewayRaceRequester) record(method sip.RequestMethod) {
	r.mu.Lock()
	switch method {
	case sip.INVITE:
		r.invites++
	case sip.ACK:
		r.acks++
	case sip.BYE:
		r.byes++
	}
	r.mu.Unlock()
}

func (r *gatewayRaceRequester) counts() (invites, acks, byes int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.invites, r.acks, r.byes
}

func realSIPServiceWithRequester(t *testing.T, requester sipgo.ClientTransactionRequester) sipmod.SIPService {
	t.Helper()
	cfg := config.Defaults()
	cfg.SIP.Listen = "127.0.0.1:0"
	cfg.SIP.Transport = nil
	cfg.SIP.ServerID = "test"
	cfg.SIP.Domain = "test.local"
	server := core.NewServer(cfg)
	sipModule := sipmod.NewModule()
	if err := sipModule.Init(server); err != nil {
		t.Fatalf("init SIP module: %v", err)
	}
	t.Cleanup(func() { _ = sipModule.Close() })

	service := sipModule.Service()
	serviceValue := reflect.ValueOf(service).Elem()
	clientValue := serviceValue.FieldByName("client").Elem().FieldByName("client")
	client := reflect.NewAt(clientValue.Type(), unsafe.Pointer(clientValue.UnsafeAddr())).Elem().Interface().(*sipgo.Client)
	client.TxRequester = requester
	return service
}

func TestGatewayFinalizesOffered2xxFromRealSIPAdapterOnCancellation(t *testing.T) {
	previousProcs := runtime.GOMAXPROCS(1)
	t.Cleanup(func() { runtime.GOMAXPROCS(previousProcs) })

	ctx, cancel := context.WithCancel(context.Background())
	clientTx := newOfferedFinalClientTx()
	requester := &gatewayRaceRequester{
		cancel:     cancel,
		inviteTx:   clientTx,
		senderDone: make(chan struct{}),
	}
	requester.ackRequest = func(_ context.Context, req *sip.Request) error {
		requester.record(req.Method)
		return nil
	}
	realService := realSIPServiceWithRequester(t, requester)

	gw, _, hub := newControlPlaneGateway(t, newTestGatewayConfig(t))
	stream, _ := hub.GetOrCreate("live/adapter-cancel-race")
	publishTestAudio(t, stream, avframe.CodecG711A)
	var inviteDone <-chan struct{}
	gw.sendInvite = func(ctx context.Context, req *sip.Request) (inviteDialog, error) {
		tx, err := realService.SendInvite(ctx, req)
		if tx != nil {
			inviteDone = tx.Done()
		}
		return tx, err
	}

	dialDone := make(chan error, 1)
	go func() {
		_, err := gw.Dial(ctx, "alice", stream.Key())
		dialDone <- err
	}()

	select {
	case err := <-dialDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Dial error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Gateway.Dial did not terminate after caller cancellation")
	}
	select {
	case <-requester.senderDone:
	case <-time.After(time.Second):
		t.Fatal("final-response sender remained blocked after Gateway.Dial returned")
	}
	select {
	case <-inviteDone:
	case <-time.After(time.Second):
		t.Fatal("real InviteTransaction collector remained blocked after Gateway.Dial returned")
	}

	invites, acks, byes := requester.counts()
	if invites != 1 || acks != 1 || byes != 1 {
		t.Fatalf("SIP signaling INVITE=%d ACK=%d BYE=%d, want exactly 1 each", invites, acks, byes)
	}
	if got := gw.ActiveCalls(); got != 0 {
		t.Fatalf("ActiveCalls = %d, want 0 after canceled 2xx", got)
	}
}

func TestGatewayBoundsBlockingRealSIPACKAndStillSendsBYE(t *testing.T) {
	previousProcs := runtime.GOMAXPROCS(1)
	t.Cleanup(func() { runtime.GOMAXPROCS(previousProcs) })

	ctx, cancel := context.WithCancel(context.Background())
	clientTx := newOfferedFinalClientTx()
	requester := &gatewayRaceRequester{
		cancel:     cancel,
		inviteTx:   clientTx,
		senderDone: make(chan struct{}),
	}
	ackStarted := make(chan struct{})
	ackDone := make(chan struct{})
	ackRelease := make(chan struct{})
	var ackStartOnce sync.Once
	var ackReleaseOnce sync.Once
	t.Cleanup(func() { ackReleaseOnce.Do(func() { close(ackRelease) }) })
	requester.ackRequest = func(ctx context.Context, req *sip.Request) error {
		requester.record(req.Method)
		ackStartOnce.Do(func() { close(ackStarted) })
		defer close(ackDone)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ackRelease:
			return errors.New("ACK test transport released")
		}
	}
	realService := realSIPServiceWithRequester(t, requester)

	gw, _, hub := newControlPlaneGateway(t, newTestGatewayConfig(t))
	stream, _ := hub.GetOrCreate("live/blocking-ack")
	publishTestAudio(t, stream, avframe.CodecG711A)
	gw.sendInvite = func(ctx context.Context, req *sip.Request) (inviteDialog, error) {
		return realService.SendInvite(ctx, req)
	}

	started := time.Now()
	dialDone := make(chan error, 1)
	go func() {
		_, err := gw.Dial(ctx, "alice", stream.Key())
		dialDone <- err
	}()
	select {
	case <-ackStarted:
	case <-time.After(time.Second):
		t.Fatal("real SIP ACK write did not start")
	}

	var dialErr error
	exceededDeadline := false
	select {
	case dialErr = <-dialDone:
	case <-time.After(6 * time.Second):
		exceededDeadline = true
		ackReleaseOnce.Do(func() { close(ackRelease) })
		dialErr = <-dialDone
	}
	if exceededDeadline {
		t.Fatal("Gateway.Dial remained blocked after the 5s ACK deadline")
	}
	if elapsed := time.Since(started); elapsed > 6*time.Second {
		t.Fatalf("Gateway.Dial elapsed %v, want at most 6s", elapsed)
	}
	if !errors.Is(dialErr, context.DeadlineExceeded) {
		t.Fatalf("Dial error = %v, want context.DeadlineExceeded", dialErr)
	}
	select {
	case <-ackDone:
	default:
		t.Fatal("Gateway.Dial returned before the blocked ACK writer terminated")
	}

	invites, acks, byes := requester.counts()
	if invites != 1 || acks != 1 || byes != 1 {
		t.Fatalf("SIP signaling INVITE=%d ACK=%d BYE=%d, want exactly 1 each", invites, acks, byes)
	}
	if got := gw.ActiveCalls(); got != 0 {
		t.Fatalf("ActiveCalls = %d, want 0 after ACK deadline", got)
	}
}

func TestGatewayRejectsOutboundSourceCodecMismatchBeforeInvite(t *testing.T) {
	gw, _, hub := newControlPlaneGateway(t, newTestGatewayConfig(t))
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
	cfg := newTestGatewayConfig(t)
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
	cfg := newTestGatewayConfig(t)
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
			gw, _, hub := newControlPlaneGateway(t, newTestGatewayConfig(t))
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
	gw, _, hub := newControlPlaneGateway(t, newTestGatewayConfig(t))
	stream, _ := hub.GetOrCreate("live/uri-invalid")
	publishTestAudio(t, stream, avframe.CodecG711A)
	invites := 0
	gw.sendInvite = func(context.Context, *sip.Request) (inviteDialog, error) {
		invites++
		return nil, errors.New("unexpected INVITE")
	}

	for _, target := range []string{
		"alice@example.com",
		"sip:alice",
		"sip:@example.com",
		"sip:alice@example.com:0",
		"sip:alice@example.com:70000",
		"sip:alice\r\nInjected@example.com",
		"sip:ali ce@example.com",
		"sip:alice@invalid_host.example.com",
		"http:alice@example.com",
	} {
		t.Run(target, func(t *testing.T) {
			if _, err := gw.Dial(context.Background(), target, stream.Key()); !errors.Is(err, ErrInvalidTargetURI) {
				t.Fatalf("Dial(%q) error = %v, want ErrInvalidTargetURI", target, err)
			}
		})
	}
	if invites != 0 {
		t.Fatalf("malformed targets sent %d INVITEs, want 0", invites)
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
			gw, _, hub := newControlPlaneGateway(t, newTestGatewayConfig(t))
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
	gw, _, hub := newControlPlaneGateway(t, newTestGatewayConfig(t))
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

	if _, err := gw.Dial(ctx, "alice", stream.Key()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Dial error = %v, want context.Canceled", err)
	}
	dialog.mu.Lock()
	acks, byes := dialog.acks, dialog.byes
	dialog.mu.Unlock()
	if acks != 1 || byes != 1 {
		t.Fatalf("dialog signaling ACK=%d BYE=%d, want 1 each", acks, byes)
	}
	if got := gw.ActiveCalls(); got != 0 {
		t.Fatalf("ActiveCalls = %d, want 0 after caller cancellation", got)
	}
}

func TestGatewayOutboundNegotiationDoesNotMixPublisherGenerations(t *testing.T) {
	gw, _, hub := newControlPlaneGateway(t, newTestGatewayConfig(t))
	stream, _ := hub.GetOrCreate("live/negotiation-generation")
	publisherA := &gatewayTestPublisher{id: "publisher-a", info: &avframe.MediaInfo{
		AudioCodec: avframe.CodecG711A,
		SampleRate: 8000,
		Channels:   1,
	}}
	if err := stream.SetPublisher(publisherA); err != nil {
		t.Fatalf("SetPublisher publisher A: %v", err)
	}

	inviteEntered := make(chan struct{})
	releaseInvite := make(chan struct{})
	dialog := &fakeInviteDialog{done: make(chan struct{})}
	gw.sendInvite = func(_ context.Context, req *sip.Request) (inviteDialog, error) {
		if !strings.Contains(string(req.Body()), "PCMA/8000") {
			t.Fatalf("SIP offer = %q, want publisher-A PCMA codec", req.Body())
		}
		close(inviteEntered)
		<-releaseInvite
		dialog.response = sip.NewResponseFromRequest(req, 200, "OK", []byte(testAudioOffer))
		close(dialog.done)
		return dialog, nil
	}

	dialDone := make(chan error, 1)
	go func() {
		_, err := gw.Dial(context.Background(), "alice", stream.Key())
		dialDone <- err
	}()
	select {
	case <-inviteEntered:
	case <-time.After(time.Second):
		t.Fatal("SIP outbound negotiation did not start")
	}

	stream.RemovePublisher()
	if err := stream.SetPublisher(&gatewayTestPublisher{id: "publisher-b", info: &avframe.MediaInfo{
		AudioCodec: avframe.CodecG711U,
		SampleRate: 8000,
		Channels:   1,
	}}); err != nil {
		t.Fatalf("SetPublisher publisher B: %v", err)
	}
	close(releaseInvite)

	select {
	case err := <-dialDone:
		if err == nil {
			t.Fatal("SIP outbound negotiation succeeded with publisher-A signaling and publisher-B media")
		}
	case <-time.After(time.Second):
		t.Fatal("SIP outbound negotiation did not terminate after publisher replacement")
	}
	dialog.mu.Lock()
	acks := dialog.acks
	dialog.mu.Unlock()
	if acks != 0 {
		t.Fatalf("stale SIP negotiation sent %d ACKs, want none", acks)
	}
	if got := gw.ActiveCalls(); got != 0 {
		t.Fatalf("stale SIP negotiation activated %d calls", got)
	}
}

func TestGatewayDialWaitsForPublisherReadinessBeforeSendingInvite(t *testing.T) {
	gw, _, hub := newControlPlaneGateway(t, newTestGatewayConfig(t))
	stream, _ := hub.GetOrCreate("live/late-header")
	if err := stream.SetPublisher(&gatewayTestPublisher{id: "late-header", info: &avframe.MediaInfo{
		VideoCodec: avframe.CodecH264,
		AudioCodec: avframe.CodecG711A,
		SampleRate: 8000,
		Channels:   1,
	}}); err != nil {
		t.Fatal(err)
	}

	inviteEntered := make(chan struct{})
	gw.sendInvite = func(ctx context.Context, _ *sip.Request) (inviteDialog, error) {
		close(inviteEntered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dialDone := make(chan error, 1)
	go func() {
		_, err := gw.Dial(ctx, "alice", stream.Key())
		dialDone <- err
	}()

	select {
	case <-inviteEntered:
		t.Fatal("SIP Dial sent INVITE before the video sequence header was ready")
	case <-time.After(50 * time.Millisecond):
	}

	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader,
		0, 0, []byte{0x01, 0x42, 0x00, 0x1e, 0xff},
	))
	select {
	case <-inviteEntered:
	case <-time.After(time.Second):
		t.Fatal("SIP Dial did not send INVITE after the publisher became ready")
	}
	cancel()
	select {
	case <-dialDone:
	case <-time.After(time.Second):
		t.Fatal("SIP Dial did not stop after test cancellation")
	}
}

func TestGatewayGenerationRetirementAfterAccepted2xxSendsBYE(t *testing.T) {
	for _, test := range []struct {
		name   string
		retire func(*core.Stream, *fakeInviteDialog)
	}{
		{
			name: "before send invite returns",
			retire: func(stream *core.Stream, _ *fakeInviteDialog) {
				stream.RemovePublisher()
			},
		},
		{
			name: "while response is observed",
			retire: func(stream *core.Stream, dialog *fakeInviteDialog) {
				var once sync.Once
				dialog.responseHook = func() {
					once.Do(func() { stream.RemovePublisher() })
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			gw, _, hub := newControlPlaneGateway(t, newTestGatewayConfig(t))
			stream, _ := hub.GetOrCreate("live/accepted-generation-retirement")
			publishTestAudio(t, stream, avframe.CodecG711A)
			dialog := &fakeInviteDialog{done: make(chan struct{})}
			close(dialog.done)
			gw.sendInvite = func(_ context.Context, req *sip.Request) (inviteDialog, error) {
				dialog.response = sip.NewResponseFromRequest(req, 200, "OK", []byte(testAudioOffer))
				test.retire(stream, dialog)
				return dialog, nil
			}

			_, err := gw.Dial(context.Background(), "alice", stream.Key())
			if err == nil || !strings.Contains(err.Error(), "publisher generation is no longer active") {
				t.Fatalf("Dial error = %v, want source-generation retirement error", err)
			}
			dialog.mu.Lock()
			acks, byes, closes := dialog.acks, dialog.byes, dialog.closes
			dialog.mu.Unlock()
			if acks != 0 || byes != 1 || closes != 1 {
				t.Fatalf("accepted generation cleanup ACK=%d BYE=%d Close=%d, want 0/1/1", acks, byes, closes)
			}
		})
	}
}

func TestGatewayFinalizesReady2xxWhenCallerAlreadyCanceled(t *testing.T) {
	gw, _, hub := newControlPlaneGateway(t, newTestGatewayConfig(t))
	stream, _ := hub.GetOrCreate("live/simultaneous-ready")
	publishTestAudio(t, stream, avframe.CodecG711A)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dialog := &fakeInviteDialog{done: make(chan struct{})}
	close(dialog.done)
	gw.sendInvite = func(_ context.Context, req *sip.Request) (inviteDialog, error) {
		dialog.response = sip.NewResponseFromRequest(req, 200, "OK", []byte(testAudioOffer))
		return dialog, nil
	}

	if _, err := gw.Dial(ctx, "alice", stream.Key()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Dial error = %v, want context.Canceled", err)
	}
	dialog.mu.Lock()
	acks, byes := dialog.acks, dialog.byes
	dialog.mu.Unlock()
	if acks != 1 || byes != 1 {
		t.Fatalf("simultaneous ready signaling ACK=%d BYE=%d, want 1 each", acks, byes)
	}
	if got := gw.ActiveCalls(); got != 0 {
		t.Fatalf("ActiveCalls = %d, want 0 after canceled 2xx", got)
	}
}

func TestGatewayTearsDownDialogWhenActivationFailsAfterACK(t *testing.T) {
	gw, _, hub := newControlPlaneGateway(t, newTestGatewayConfig(t))
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
	gw, _, hub := newControlPlaneGateway(t, newTestGatewayConfig(t))
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
	gw, _, hub := newControlPlaneGateway(t, newTestGatewayConfig(t))
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
	gw, _, hub := newControlPlaneGateway(t, newTestGatewayConfig(t))
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
	gw, _, hub := newControlPlaneGateway(t, newTestGatewayConfig(t))
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
	cfg := newTestGatewayConfig(t)
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
	gw, _, _ := newControlPlaneGateway(t, newTestGatewayConfig(t))
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
