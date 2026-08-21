package sipgateway

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	sipmod "github.com/im-pingo/liveforge/module/sip"
	"github.com/im-pingo/liveforge/pkg/sdp"
)

type authorizerFunc func(context.Context, core.AuthorizationRequest) error

func (f authorizerFunc) Authorize(ctx context.Context, req core.AuthorizationRequest) error {
	return f(ctx, req)
}

type mockSIPService struct {
	mu             sync.Mutex
	inviteHandlers []sipmod.InviteHandler
	byeHandlers    []sipmod.ByeHandler
	localAddr      string
	serverID       string
	domain         string
}

func (m *mockSIPService) OnRegister(h sipmod.RegisterHandler) {}
func (m *mockSIPService) OnInvite(h sipmod.InviteHandler) {
	m.mu.Lock()
	m.inviteHandlers = append(m.inviteHandlers, h)
	m.mu.Unlock()
}
func (m *mockSIPService) OnBye(h sipmod.ByeHandler) {
	m.mu.Lock()
	m.byeHandlers = append(m.byeHandlers, h)
	m.mu.Unlock()
}
func (m *mockSIPService) OnMessage(h sipmod.MessageHandler)     {}
func (m *mockSIPService) OnSubscribe(h sipmod.SubscribeHandler) {}
func (m *mockSIPService) OnNotify(h sipmod.NotifyHandler)       {}
func (m *mockSIPService) SendRequest(ctx context.Context, req *sip.Request) (*sip.Response, error) {
	return nil, nil
}
func (m *mockSIPService) SendInvite(ctx context.Context, req *sip.Request) (*sipmod.InviteTransaction, error) {
	return nil, nil
}
func (m *mockSIPService) LocalAddr() string { return m.localAddr }
func (m *mockSIPService) ServerID() string  { return m.serverID }
func (m *mockSIPService) Domain() string    { return m.domain }

type mockServerTx struct {
	mu       sync.Mutex
	response *sip.Response
}

func (tx *mockServerTx) Respond(resp *sip.Response) error {
	tx.mu.Lock()
	tx.response = resp
	tx.mu.Unlock()
	return nil
}

func (tx *mockServerTx) Acks() <-chan *sip.Request            { return nil }
func (tx *mockServerTx) Done() <-chan struct{}                { return nil }
func (tx *mockServerTx) Terminate()                           {}
func (tx *mockServerTx) Err() error                           { return nil }
func (tx *mockServerTx) OnTerminate(f sip.FnTxTerminate) bool { return true }
func (tx *mockServerTx) OnCancel(f sip.FnTxCancel) bool       { return true }

func (tx *mockServerTx) getResponse() *sip.Response {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	return tx.response
}

func newTestHub() *core.StreamHub {
	bus := core.NewEventBus()
	return core.NewStreamHub(config.StreamConfig{RingBufferSize: 256}, config.LimitsConfig{}, bus)
}

func newTestGatewayConfig() config.SIPGatewayConfig {
	return config.SIPGatewayConfig{
		Enabled:      true,
		StreamPrefix: "sip",
		RTPPortRange: []int{40000, 40100},
		Codecs:       []string{"PCMA", "PCMU"},
		MaxCalls:     10,
	}
}

func TestNewGateway(t *testing.T) {
	sipSvc := &mockSIPService{localAddr: "127.0.0.1:5060", serverID: "test", domain: "test.local"}
	hub := newTestHub()
	bus := core.NewEventBus()

	gw, err := NewGateway(newTestGatewayConfig(), sipSvc, hub, bus)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	defer gw.Close()

	if gw.ActiveCalls() != 0 {
		t.Errorf("ActiveCalls = %d, want 0", gw.ActiveCalls())
	}
}

func TestNewGatewayBadPortRange(t *testing.T) {
	sipSvc := &mockSIPService{localAddr: "127.0.0.1:5060"}
	hub := newTestHub()
	bus := core.NewEventBus()

	cfg := newTestGatewayConfig()
	cfg.RTPPortRange = []int{100}

	_, err := NewGateway(cfg, sipSvc, hub, bus)
	if err == nil {
		t.Error("expected error for bad port range")
	}
}

func TestNegotiateCodec(t *testing.T) {
	tests := []struct {
		name      string
		offer     *sdp.MediaDescription
		preferred []string
		wantCodec string
		wantOK    bool
	}{
		{
			name: "PCMA offered and preferred",
			offer: &sdp.MediaDescription{
				Type:    "audio",
				Formats: []int{8, 0},
				Attributes: []sdp.Attribute{
					{Key: "rtpmap", Value: "8 PCMA/8000"},
					{Key: "rtpmap", Value: "0 PCMU/8000"},
				},
			},
			preferred: []string{"PCMA", "PCMU"},
			wantCodec: "PCMA",
			wantOK:    true,
		},
		{
			name: "PCMU preferred over PCMA",
			offer: &sdp.MediaDescription{
				Type:    "audio",
				Formats: []int{8, 0},
				Attributes: []sdp.Attribute{
					{Key: "rtpmap", Value: "8 PCMA/8000"},
					{Key: "rtpmap", Value: "0 PCMU/8000"},
				},
			},
			preferred: []string{"PCMU", "PCMA"},
			wantCodec: "PCMU",
			wantOK:    true,
		},
		{
			name: "static PT without rtpmap",
			offer: &sdp.MediaDescription{
				Type:    "audio",
				Formats: []int{0},
			},
			preferred: []string{"PCMU"},
			wantCodec: "PCMU",
			wantOK:    true,
		},
		{
			name: "opus dynamic PT",
			offer: &sdp.MediaDescription{
				Type:    "audio",
				Formats: []int{111},
				Attributes: []sdp.Attribute{
					{Key: "rtpmap", Value: "111 opus/48000/2"},
				},
			},
			preferred: []string{"opus"},
			wantCodec: "opus",
			wantOK:    true,
		},
		{
			name: "no common codec",
			offer: &sdp.MediaDescription{
				Type:    "audio",
				Formats: []int{96},
				Attributes: []sdp.Attribute{
					{Key: "rtpmap", Value: "96 CUSTOM/16000"},
				},
			},
			preferred: []string{"PCMA", "PCMU"},
			wantOK:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nc, ok := negotiateCodec(tt.offer, tt.preferred)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && nc.EncodingName != tt.wantCodec {
				t.Errorf("codec = %q, want %q", nc.EncodingName, tt.wantCodec)
			}
		})
	}
}

func TestBuildAnswerSDP(t *testing.T) {
	nc := negotiatedCodec{
		Codec:        8, // CodecG711A
		PT:           8,
		ClockRate:    8000,
		EncodingName: "PCMA",
	}

	body := buildAnswerSDP("192.168.1.1", 40000, nc)

	sd, err := sdp.Parse(body)
	if err != nil {
		t.Fatalf("Parse answer SDP: %v", err)
	}

	if len(sd.Media) != 1 {
		t.Fatalf("expected 1 media section, got %d", len(sd.Media))
	}

	m := sd.Media[0]
	if m.Type != "audio" {
		t.Errorf("media type = %q, want audio", m.Type)
	}
	if m.Port != 40000 {
		t.Errorf("port = %d, want 40000", m.Port)
	}
	if len(m.Formats) != 1 || m.Formats[0] != 8 {
		t.Errorf("formats = %v, want [8]", m.Formats)
	}
}

func TestBuildOfferSDP(t *testing.T) {
	codecs := []negotiatedCodec{
		{Codec: 8, PT: 8, ClockRate: 8000, EncodingName: "PCMA"},
		{Codec: 0, PT: 0, ClockRate: 8000, EncodingName: "PCMU"},
	}

	body := buildOfferSDP("10.0.0.1", 40002, codecs)

	sd, err := sdp.Parse(body)
	if err != nil {
		t.Fatalf("Parse offer SDP: %v", err)
	}

	if len(sd.Media) != 1 {
		t.Fatalf("expected 1 media, got %d", len(sd.Media))
	}

	m := sd.Media[0]
	if len(m.Formats) != 2 {
		t.Errorf("formats = %v, want 2 entries", m.Formats)
	}
}

func TestGatewayHandleInviteSuccess(t *testing.T) {
	sipSvc := &mockSIPService{localAddr: "127.0.0.1:5060", serverID: "test", domain: "test.local"}
	hub := newTestHub()
	bus := core.NewEventBus()

	gw, err := NewGateway(newTestGatewayConfig(), sipSvc, hub, bus)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	defer gw.Close()

	offerSDP := "v=0\r\no=- 1 1 IN IP4 192.168.1.100\r\ns=-\r\nc=IN IP4 192.168.1.100\r\nt=0 0\r\nm=audio 40000 RTP/AVP 8 0\r\na=rtpmap:8 PCMA/8000\r\na=rtpmap:0 PCMU/8000\r\n"

	req := sip.NewRequest(sip.INVITE, sip.Uri{User: "teststream", Host: "test.local"})
	req.AppendHeader(sip.NewHeader("Call-ID", "test-call-123"))
	req.SetBody([]byte(offerSDP))

	tx := &mockServerTx{}

	sipSvc.mu.Lock()
	handlers := make([]sipmod.InviteHandler, len(sipSvc.inviteHandlers))
	copy(handlers, sipSvc.inviteHandlers)
	sipSvc.mu.Unlock()

	for _, h := range handlers {
		h(req, tx)
	}

	resp := tx.getResponse()
	if resp == nil {
		t.Fatal("no response sent")
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	if gw.ActiveCalls() != 1 {
		t.Errorf("ActiveCalls = %d, want 1", gw.ActiveCalls())
	}

	_, ok := hub.Find("sip/teststream")
	if !ok {
		t.Error("stream sip/teststream not found in hub")
	}
}

func TestGatewayHandleInviteAuthorizationRejectsBeforeResources(t *testing.T) {
	cfg := config.Config{Stream: config.StreamConfig{RingBufferSize: 16}}
	server := core.NewServer(&cfg)
	var stages []core.AuthorizationStage
	server.SetAuthorizer(authorizerFunc(func(_ context.Context, req core.AuthorizationRequest) error {
		stages = append(stages, req.Stage)
		return fmt.Errorf("denied")
	}))
	sipSvc := &mockSIPService{localAddr: "127.0.0.1:5060", serverID: "test", domain: "test.local"}
	hub := server.StreamHub()
	bus := server.GetEventBus()
	gateway, err := NewGateway(newTestGatewayConfig(), sipSvc, hub, bus, server)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	defer gateway.Close()

	req := sip.NewRequest(sip.INVITE, sip.Uri{User: "denied", Host: "test.local"})
	req.AppendHeader(sip.NewHeader("Call-ID", "denied-call"))
	req.SetBody([]byte("v=0\r\nm=audio 40000 RTP/AVP 8\r\na=rtpmap:8 PCMA/8000\r\n"))
	tx := &mockServerTx{}
	gateway.handleInvite(req, tx)

	if resp := tx.getResponse(); resp == nil || resp.StatusCode != 401 {
		t.Fatalf("response = %+v, want 401", resp)
	}
	if len(stages) != 1 || stages[0] != core.AuthorizationPreSession {
		t.Fatalf("authorization stages = %v, want [%s]", stages, core.AuthorizationPreSession)
	}
	if hub.Count() != 0 || gateway.ActiveCalls() != 0 {
		t.Fatalf("rejected INVITE committed streams=%d calls=%d", hub.Count(), gateway.ActiveCalls())
	}
	rtpPort, rtcpPort, err := gateway.portAlloc.AllocatePair()
	if err != nil {
		t.Fatalf("rejected INVITE leaked RTP ports: %v", err)
	}
	gateway.portAlloc.Free(rtpPort, rtcpPort)
}

func TestGatewayHandleInvitePostConnectAuthorizationRejectsBeforeResources(t *testing.T) {
	cfg := config.Config{Stream: config.StreamConfig{RingBufferSize: 16}}
	server := core.NewServer(&cfg)
	var stages []core.AuthorizationStage
	server.SetAuthorizer(authorizerFunc(func(_ context.Context, req core.AuthorizationRequest) error {
		stages = append(stages, req.Stage)
		if req.Stage == core.AuthorizationPostConnect {
			return fmt.Errorf("denied")
		}
		return nil
	}))
	sipSvc := &mockSIPService{localAddr: "127.0.0.1:5060", serverID: "test", domain: "test.local"}
	gateway, err := NewGateway(newTestGatewayConfig(), sipSvc, server.StreamHub(), server.GetEventBus(), server)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	defer gateway.Close()

	req := sip.NewRequest(sip.INVITE, sip.Uri{User: "denied-post", Host: "test.local"})
	req.AppendHeader(sip.NewHeader("Call-ID", "denied-post-call"))
	req.SetBody([]byte("v=0\r\nm=audio 40000 RTP/AVP 8\r\na=rtpmap:8 PCMA/8000\r\n"))
	tx := &mockServerTx{}
	gateway.handleInvite(req, tx)

	if resp := tx.getResponse(); resp == nil || resp.StatusCode != 401 {
		t.Fatalf("response = %+v, want 401", resp)
	}
	if got := fmt.Sprint(stages); got != fmt.Sprint([]core.AuthorizationStage{core.AuthorizationPreSession, core.AuthorizationPostConnect}) {
		t.Fatalf("authorization stages = %v, want pre and post", stages)
	}
	if server.StreamHub().Count() != 0 || gateway.ActiveCalls() != 0 {
		t.Fatalf("rejected INVITE committed streams=%d calls=%d", server.StreamHub().Count(), gateway.ActiveCalls())
	}
}

func TestGatewayIgnoresVideoInviteForGB28181(t *testing.T) {
	sipSvc := &mockSIPService{localAddr: "127.0.0.1:5060", serverID: "test", domain: "test.local"}
	hub := newTestHub()
	bus := core.NewEventBus()

	gw, err := NewGateway(newTestGatewayConfig(), sipSvc, hub, bus)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	defer gw.Close()

	offerSDP := "v=0\r\no=- 1 1 IN IP4 192.168.1.100\r\ns=-\r\nc=IN IP4 192.168.1.100\r\nt=0 0\r\nm=video 40000 RTP/AVP 96\r\na=rtpmap:96 H264/90000\r\n"

	req := sip.NewRequest(sip.INVITE, sip.Uri{User: "teststream", Host: "test.local"})
	req.AppendHeader(sip.NewHeader("Call-ID", "test-call-video"))
	req.SetBody([]byte(offerSDP))

	tx := &mockServerTx{}

	sipSvc.mu.Lock()
	handlers := make([]sipmod.InviteHandler, len(sipSvc.inviteHandlers))
	copy(handlers, sipSvc.inviteHandlers)
	sipSvc.mu.Unlock()

	for _, h := range handlers {
		h(req, tx)
	}

	if resp := tx.getResponse(); resp != nil {
		t.Fatalf("video INVITE response = %d, want no response from audio gateway", resp.StatusCode)
	}
}

func TestGatewayHandleBye(t *testing.T) {
	sipSvc := &mockSIPService{localAddr: "127.0.0.1:5060", serverID: "test", domain: "test.local"}
	hub := newTestHub()
	bus := core.NewEventBus()

	gw, err := NewGateway(newTestGatewayConfig(), sipSvc, hub, bus)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	defer gw.Close()

	// First establish a call via INVITE
	offerSDP := "v=0\r\no=- 1 1 IN IP4 192.168.1.100\r\ns=-\r\nc=IN IP4 192.168.1.100\r\nt=0 0\r\nm=audio 40000 RTP/AVP 8\r\na=rtpmap:8 PCMA/8000\r\n"

	req := sip.NewRequest(sip.INVITE, sip.Uri{User: "byetest", Host: "test.local"})
	req.AppendHeader(sip.NewHeader("Call-ID", "bye-call-456"))
	req.SetBody([]byte(offerSDP))

	inviteTx := &mockServerTx{}

	sipSvc.mu.Lock()
	inviteHandlers := make([]sipmod.InviteHandler, len(sipSvc.inviteHandlers))
	copy(inviteHandlers, sipSvc.inviteHandlers)
	sipSvc.mu.Unlock()
	for _, h := range inviteHandlers {
		h(req, inviteTx)
	}

	if gw.ActiveCalls() != 1 {
		t.Fatalf("ActiveCalls after INVITE = %d, want 1", gw.ActiveCalls())
	}

	// Now send BYE
	byeReq := sip.NewRequest(sip.BYE, sip.Uri{User: "byetest", Host: "test.local"})
	byeReq.AppendHeader(sip.NewHeader("Call-ID", "bye-call-456"))

	byeTx := &mockServerTx{}

	sipSvc.mu.Lock()
	byeHandlers := make([]sipmod.ByeHandler, len(sipSvc.byeHandlers))
	copy(byeHandlers, sipSvc.byeHandlers)
	sipSvc.mu.Unlock()
	for _, h := range byeHandlers {
		h(byeReq, byeTx)
	}

	time.Sleep(50 * time.Millisecond)

	if gw.ActiveCalls() != 0 {
		t.Errorf("ActiveCalls after BYE = %d, want 0", gw.ActiveCalls())
	}

	byeResp := byeTx.getResponse()
	if byeResp == nil || byeResp.StatusCode != 200 {
		t.Error("expected 200 OK for BYE")
	}
}

func TestGatewayMaxCalls(t *testing.T) {
	sipSvc := &mockSIPService{localAddr: "127.0.0.1:5060", serverID: "test", domain: "test.local"}
	hub := newTestHub()
	bus := core.NewEventBus()

	cfg := newTestGatewayConfig()
	cfg.MaxCalls = 1

	gw, err := NewGateway(cfg, sipSvc, hub, bus)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	defer gw.Close()

	offerSDP := "v=0\r\no=- 1 1 IN IP4 192.168.1.100\r\ns=-\r\nc=IN IP4 192.168.1.100\r\nt=0 0\r\nm=audio 40000 RTP/AVP 8\r\na=rtpmap:8 PCMA/8000\r\n"

	sipSvc.mu.Lock()
	inviteHandlers := make([]sipmod.InviteHandler, len(sipSvc.inviteHandlers))
	copy(inviteHandlers, sipSvc.inviteHandlers)
	sipSvc.mu.Unlock()

	// First call
	req1 := sip.NewRequest(sip.INVITE, sip.Uri{User: "call1", Host: "test.local"})
	req1.AppendHeader(sip.NewHeader("Call-ID", "max-call-1"))
	req1.SetBody([]byte(offerSDP))
	tx1 := &mockServerTx{}
	for _, h := range inviteHandlers {
		h(req1, tx1)
	}

	// Second call should be rejected
	req2 := sip.NewRequest(sip.INVITE, sip.Uri{User: "call2", Host: "test.local"})
	req2.AppendHeader(sip.NewHeader("Call-ID", "max-call-2"))
	req2.SetBody([]byte(offerSDP))
	tx2 := &mockServerTx{}
	for _, h := range inviteHandlers {
		h(req2, tx2)
	}

	resp2 := tx2.getResponse()
	if resp2 == nil || resp2.StatusCode != 503 {
		status := 0
		if resp2 != nil {
			status = resp2.StatusCode
		}
		t.Errorf("second call status = %d, want 503", status)
	}
}

func TestGatewayClose(t *testing.T) {
	sipSvc := &mockSIPService{localAddr: "127.0.0.1:5060", serverID: "test", domain: "test.local"}
	hub := newTestHub()
	bus := core.NewEventBus()

	gw, err := NewGateway(newTestGatewayConfig(), sipSvc, hub, bus)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}

	// Establish a call
	offerSDP := "v=0\r\no=- 1 1 IN IP4 192.168.1.100\r\ns=-\r\nc=IN IP4 192.168.1.100\r\nt=0 0\r\nm=audio 40000 RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\n"

	req := sip.NewRequest(sip.INVITE, sip.Uri{User: "closetest", Host: "test.local"})
	req.AppendHeader(sip.NewHeader("Call-ID", "close-call-789"))
	req.SetBody([]byte(offerSDP))

	tx := &mockServerTx{}
	sipSvc.mu.Lock()
	handlers := make([]sipmod.InviteHandler, len(sipSvc.inviteHandlers))
	copy(handlers, sipSvc.inviteHandlers)
	sipSvc.mu.Unlock()
	for _, h := range handlers {
		h(req, tx)
	}

	if gw.ActiveCalls() != 1 {
		t.Fatalf("ActiveCalls = %d, want 1", gw.ActiveCalls())
	}

	gw.Close()

	if gw.ActiveCalls() != 0 {
		t.Errorf("ActiveCalls after close = %d, want 0", gw.ActiveCalls())
	}
	if stream, ok := hub.Find("sip/closetest"); ok && stream.Publisher() != nil {
		t.Fatal("gateway close left inbound publisher attached")
	}
}

func TestModuleName(t *testing.T) {
	m := NewModule(&mockSIPService{})
	if m.Name() != "sipgateway" {
		t.Errorf("Name = %q, want sipgateway", m.Name())
	}
}

func TestModuleCloseNilGateway(t *testing.T) {
	m := NewModule(&mockSIPService{})
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestHangupUnknownCall(t *testing.T) {
	sipSvc := &mockSIPService{localAddr: "127.0.0.1:5060", serverID: "test", domain: "test.local"}
	hub := newTestHub()
	bus := core.NewEventBus()

	gw, err := NewGateway(newTestGatewayConfig(), sipSvc, hub, bus)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	defer gw.Close()

	if err := gw.Hangup("nonexistent"); err == nil {
		t.Error("expected error for unknown call")
	}
}

func TestRemoteAddress(t *testing.T) {
	tests := []struct {
		name string
		sd   *sdp.SessionDescription
		want string
	}{
		{
			name: "session-level connection",
			sd: &sdp.SessionDescription{
				Connection: &sdp.Connection{Address: "10.0.0.1"},
			},
			want: "10.0.0.1",
		},
		{
			name: "media-level connection",
			sd: &sdp.SessionDescription{
				Media: []*sdp.MediaDescription{
					{Connection: &sdp.Connection{Address: "10.0.0.2"}},
				},
			},
			want: "10.0.0.2",
		},
		{
			name: "no connection",
			sd:   &sdp.SessionDescription{},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := remoteAddress(tt.sd)
			if got != tt.want {
				t.Errorf("remoteAddress = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLocalAddress(t *testing.T) {
	if got := localAddress("192.168.1.1:5060"); got != "192.168.1.1" {
		t.Errorf("localAddress(192.168.1.1:5060) = %q", got)
	}
}
