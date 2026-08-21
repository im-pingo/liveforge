package gb28181

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	sipmod "github.com/im-pingo/liveforge/module/sip"
	"github.com/im-pingo/liveforge/pkg/portalloc"
)

type captureServerTransaction struct {
	response *sip.Response
	done     chan struct{}
}

func (t *captureServerTransaction) Terminate()                         {}
func (t *captureServerTransaction) OnTerminate(sip.FnTxTerminate) bool { return true }
func (t *captureServerTransaction) Done() <-chan struct{}              { return t.done }
func (t *captureServerTransaction) Err() error                         { return nil }
func (t *captureServerTransaction) Respond(resp *sip.Response) error {
	t.response = resp
	return nil
}
func (t *captureServerTransaction) Acks() <-chan *sip.Request { return make(chan *sip.Request) }
func (t *captureServerTransaction) OnCancel(sip.FnTxCancel) bool {
	return true
}

type gbAuthorizerFunc func(context.Context, core.AuthorizationRequest) error

func (f gbAuthorizerFunc) Authorize(ctx context.Context, request core.AuthorizationRequest) error {
	return f(ctx, request)
}

func TestInviteAuthorizationRejectionCommitsNoMediaResources(t *testing.T) {
	for _, rejectStage := range []core.AuthorizationStage{
		core.AuthorizationPreSession,
		core.AuthorizationPostConnect,
	} {
		t.Run(string(rejectStage), func(t *testing.T) {
			cfg := &config.Config{Stream: config.StreamConfig{RingBufferSize: 16}}
			server := core.NewServer(cfg)
			var stages []core.AuthorizationStage
			server.SetAuthorizer(gbAuthorizerFunc(func(_ context.Context, request core.AuthorizationRequest) error {
				stages = append(stages, request.Stage)
				if request.Action != core.AuthorizationPublish || request.Protocol != "gb28181" ||
					request.StreamKey != "gb/channel-1" {
					t.Fatalf("authorization request = %+v", request)
				}
				if request.Stage == rejectStage {
					return errors.New("denied")
				}
				return nil
			}))
			ports, err := portalloc.New(42000, 42001)
			if err != nil {
				t.Fatal(err)
			}
			sessions := NewSessionManager()
			h := &handler{
				server: server, sessions: sessions, hub: server.StreamHub(),
				bus: server.GetEventBus(), ports: ports, prefix: "gb",
			}
			req := sip.NewRequest(sip.INVITE, sip.Uri{User: "channel-1", Host: "server.example"})
			req.AppendHeader(&sip.FromHeader{Address: sip.Uri{User: "device-1", Host: "device.example"}})
			req.AppendHeader(&sip.ToHeader{Address: sip.Uri{User: "channel-1", Host: "server.example"}})
			req.AppendHeader(sip.NewHeader("Call-ID", "call-1"))
			req.SetSource("127.0.0.1:5060")
			req.SetBody([]byte("v=0\r\nm=video 30000 RTP/AVP 96\r\n"))
			tx := &captureServerTransaction{done: make(chan struct{})}

			h.handleInvite(req, tx)

			if tx.response == nil || tx.response.StatusCode != 401 {
				t.Fatalf("INVITE response = %+v, want 401", tx.response)
			}
			wantStages := []core.AuthorizationStage{core.AuthorizationPreSession}
			if rejectStage == core.AuthorizationPostConnect {
				wantStages = append(wantStages, core.AuthorizationPostConnect)
			}
			if fmt.Sprint(stages) != fmt.Sprint(wantStages) {
				t.Fatalf("authorization stages = %v, want %v", stages, wantStages)
			}
			if server.StreamHub().Count() != 0 || len(sessions.All()) != 0 {
				t.Fatalf("rejected INVITE committed streams=%d sessions=%d", server.StreamHub().Count(), len(sessions.All()))
			}
			rtpPort, rtcpPort, err := ports.AllocatePair()
			if err != nil {
				t.Fatalf("rejected INVITE leaked RTP ports: %v", err)
			}
			defer ports.Free(rtpPort, rtcpPort)
			if rtpPort != 42000 || rtcpPort != 42001 {
				t.Fatalf("available RTP pair = %d/%d, want 42000/42001", rtpPort, rtcpPort)
			}
		})
	}
}

func TestRegisterDigestChallengesBeforeRegisteringDevice(t *testing.T) {
	const (
		deviceID = "34020000001320000001"
		realm    = "3402000000"
		password = "12345678"
	)
	registry := NewDeviceRegistry(time.Minute, "")
	h := &handler{
		registry: registry,
		auth:     sipmod.NewDigestAuth(realm, password),
	}
	req := newRegisterRequest(deviceID, realm)
	tx := &captureServerTransaction{done: make(chan struct{})}
	h.handleRegister(req, tx)

	if tx.response == nil || tx.response.StatusCode != 401 {
		t.Fatalf("first REGISTER status = %v, want 401", tx.response)
	}
	if registry.Get(deviceID) != nil {
		t.Fatal("device was registered before digest authorization")
	}
	nonce := parseAuthHeader(tx.response.GetHeader("WWW-Authenticate").Value())["nonce"]
	uri := "sip:34020000002000000001@" + realm
	req.AppendHeader(sip.NewHeader("Authorization", digestHeader(deviceID, realm, password, nonce, uri)))
	tx.response = nil
	h.handleRegister(req, tx)

	if tx.response == nil || tx.response.StatusCode != 200 {
		t.Fatalf("authorized REGISTER status = %v, want 200", tx.response)
	}
	if registry.Get(deviceID) == nil {
		t.Fatal("authorized device was not registered")
	}
}

func TestRegisterDigestCanBeToggledAtRuntime(t *testing.T) {
	registry := NewDeviceRegistry(time.Minute, "")
	h := &handler{registry: registry}
	h.setDigestAuth(config.SIPConfig{
		Domain: "realm",
		Auth:   config.SIPAuth{Enabled: true, Password: "password"},
	})

	deviceID := "34020000001320000002"
	req := newRegisterRequest(deviceID, "realm")
	tx := &captureServerTransaction{done: make(chan struct{})}
	h.handleRegister(req, tx)
	if tx.response == nil || tx.response.StatusCode != 401 {
		t.Fatalf("enabled digest REGISTER status = %v, want 401", tx.response)
	}
	if registry.Get(deviceID) != nil {
		t.Fatal("device registered while digest was enabled")
	}

	h.setDigestAuth(config.SIPConfig{Domain: "realm"})
	tx.response = nil
	h.handleRegister(req, tx)
	if tx.response == nil || tx.response.StatusCode != 200 {
		t.Fatalf("disabled digest REGISTER status = %v, want 200", tx.response)
	}
	if registry.Get(deviceID) == nil {
		t.Fatal("device was not registered after digest was disabled")
	}
}

func TestGBControlRequestsRequireDigestWhenEnabled(t *testing.T) {
	registry := NewDeviceRegistry(time.Minute, "")
	sessions := NewSessionManager()
	h := &handler{
		registry: registry,
		sessions: sessions,
	}
	h.setDigestAuth(config.SIPConfig{
		Domain: "realm",
		Auth:   config.SIPAuth{Enabled: true, Password: "password"},
	})

	tests := []struct {
		name        string
		call        func(*handler, *sip.Request, sip.ServerTransaction)
		body        []byte
		makeRequest func() *sip.Request
	}{
		{
			name: "invite",
			call: (*handler).handleInvite,
			body: []byte("v=0\r\nm=video 30000 RTP/AVP 96\r\n"),
			makeRequest: func() *sip.Request {
				req := sip.NewRequest(sip.INVITE, sip.Uri{User: "channel-1", Host: "server.example"})
				req.AppendHeader(&sip.FromHeader{Address: sip.Uri{User: "device-1", Host: "device.example"}})
				req.AppendHeader(&sip.ToHeader{Address: sip.Uri{User: "channel-1", Host: "server.example"}})
				req.SetSource("127.0.0.1:5060")
				return req
			},
		},
		{
			name: "bye",
			call: (*handler).handleBye,
			makeRequest: func() *sip.Request {
				req := sip.NewRequest(sip.BYE, sip.Uri{Host: "server.example"})
				req.AppendHeader(sip.NewHeader("Call-ID", "missing-call"))
				req.SetSource("127.0.0.1:5060")
				return req
			},
		},
		{
			name: "message",
			call: (*handler).handleMessage,
			body: []byte("<Notify>test</Notify>"),
			makeRequest: func() *sip.Request {
				req := sip.NewRequest(sip.MESSAGE, sip.Uri{Host: "server.example"})
				req.SetSource("127.0.0.1:5060")
				return req
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := tt.makeRequest()
			req.SetBody(tt.body)
			tx := &captureServerTransaction{done: make(chan struct{})}
			tt.call(h, req, tx)
			if tx.response == nil || tx.response.StatusCode != 401 {
				t.Fatalf("response = %+v, want 401", tx.response)
			}
		})
	}
}

func newRegisterRequest(deviceID, realm string) *sip.Request {
	req := sip.NewRequest(sip.REGISTER, sip.Uri{User: "34020000002000000001", Host: realm})
	req.AppendHeader(&sip.FromHeader{Address: sip.Uri{User: deviceID, Host: realm}})
	req.SetSource("127.0.0.1:5060")
	return req
}

func digestHeader(username, realm, password, nonce, uri string) string {
	ha1 := digestMD5(username + ":" + realm + ":" + password)
	ha2 := digestMD5("REGISTER:" + uri)
	response := digestMD5(ha1 + ":" + nonce + ":" + ha2)
	return fmt.Sprintf(
		`Digest username="%s", realm="%s", nonce="%s", uri="%s", response="%s", algorithm=MD5`,
		username, realm, nonce, uri, response,
	)
}

func digestMD5(value string) string {
	sum := md5.Sum([]byte(value))
	return hex.EncodeToString(sum[:])
}

func parseAuthHeader(value string) map[string]string {
	result := make(map[string]string)
	value = strings.TrimPrefix(value, "Digest ")
	for _, item := range strings.Split(value, ",") {
		parts := strings.SplitN(strings.TrimSpace(item), "=", 2)
		if len(parts) == 2 {
			result[parts[0]] = strings.Trim(parts[1], `"`)
		}
	}
	return result
}
