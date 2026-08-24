package gb28181

import (
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	sipmod "github.com/im-pingo/liveforge/module/sip"
	"github.com/im-pingo/liveforge/pkg/portalloc"
)

type captureServerTransaction struct {
	mu       sync.Mutex
	response *sip.Response
}

func (t *captureServerTransaction) Respond(response *sip.Response) error {
	t.mu.Lock()
	t.response = response
	t.mu.Unlock()
	return nil
}
func (t *captureServerTransaction) Acks() <-chan *sip.Request {
	return make(chan *sip.Request)
}
func (t *captureServerTransaction) OnCancel(sip.FnTxCancel) bool       { return false }
func (t *captureServerTransaction) Terminate()                         {}
func (t *captureServerTransaction) OnTerminate(sip.FnTxTerminate) bool { return false }
func (t *captureServerTransaction) Done() <-chan struct{}              { return make(chan struct{}) }
func (t *captureServerTransaction) Err() error                         { return nil }

func (t *captureServerTransaction) lastResponse() *sip.Response {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.response
}

func newGBRequest(method sip.RequestMethod, deviceID, channelID string) *sip.Request {
	uri := sip.Uri{User: channelID, Host: "127.0.0.1"}
	req := sip.NewRequest(method, uri)
	req.AppendHeader(sip.NewHeader("Via", "SIP/2.0/UDP 127.0.0.1:5060;branch=z9hG4bK-test"))
	req.AppendHeader(sip.NewHeader("From", fmt.Sprintf("<sip:%s@127.0.0.1>;tag=from", deviceID)))
	req.AppendHeader(sip.NewHeader("To", fmt.Sprintf("<sip:%s@127.0.0.1>", channelID)))
	req.AppendHeader(sip.NewHeader("Call-ID", "call-security-test"))
	req.AppendHeader(sip.NewHeader("CSeq", fmt.Sprintf("1 %s", method)))
	req.SetSource("127.0.0.1:5060")
	return req
}

func digestResponse(username, realm, password, method, uri, nonce string) string {
	hash := func(value string) string {
		sum := md5.Sum([]byte(value))
		return hex.EncodeToString(sum[:])
	}
	ha1 := hash(username + ":" + realm + ":" + password)
	ha2 := hash(method + ":" + uri)
	return hash(ha1 + ":" + nonce + ":" + ha2)
}

func TestRegisterRequiresDigestBeforeRegistryMutation(t *testing.T) {
	const (
		deviceID = "34020000001110000001"
		realm    = "3402000000"
		password = "secret"
	)
	registry := NewDeviceRegistry(time.Minute, "")
	h := &handler{
		registry: registry,
		auth:     sipmod.NewDigestAuth(realm, password),
	}

	unauthorized := newGBRequest(sip.REGISTER, deviceID, deviceID)
	tx := &captureServerTransaction{}
	h.handleRegister(unauthorized, tx)
	response := tx.lastResponse()
	if response == nil || response.StatusCode != 401 {
		t.Fatalf("REGISTER without digest response = %#v, want 401", response)
	}
	if response.GetHeader("WWW-Authenticate") == nil {
		t.Fatal("401 response missing WWW-Authenticate challenge")
	}
	if registry.Get(deviceID) != nil {
		t.Fatal("unauthorized REGISTER mutated device registry")
	}

	authorized := newGBRequest(sip.REGISTER, deviceID, deviceID)
	nonce := "known-nonce"
	uri := authorized.Recipient.String()
	responseHash := digestResponse(deviceID, realm, password, string(sip.REGISTER), uri, nonce)
	authorized.AppendHeader(sip.NewHeader("Authorization", fmt.Sprintf(
		`Digest username="%s", realm="%s", nonce="%s", uri="%s", response="%s"`,
		deviceID, realm, nonce, uri, responseHash,
	)))
	tx = &captureServerTransaction{}
	h.handleRegister(authorized, tx)
	if response := tx.lastResponse(); response == nil || response.StatusCode != 200 {
		t.Fatalf("REGISTER with digest response = %#v, want 200", response)
	}
	if registry.Get(deviceID) == nil {
		t.Fatal("valid digest did not register device")
	}

	unregister := newGBRequest(sip.REGISTER, deviceID, deviceID)
	unregister.AppendHeader(sip.NewHeader("Expires", "0"))
	tx = &captureServerTransaction{}
	h.handleRegister(unregister, tx)
	if response := tx.lastResponse(); response == nil || response.StatusCode != 401 {
		t.Fatalf("unauthorized unregister response = %#v, want 401", response)
	}
	if registry.Get(deviceID) == nil {
		t.Fatal("unauthorized unregister removed device")
	}
}

func TestInboundInviteRejectsAuthorizationBeforeResourceMutation(t *testing.T) {
	bus := core.NewEventBus()
	hub := core.NewStreamHub(config.StreamConfig{RingBufferSize: 16}, config.LimitsConfig{}, bus)
	ports, err := portalloc.New(42000, 42001)
	if err != nil {
		t.Fatal(err)
	}
	allocatedRTP, allocatedRTCP, err := ports.AllocatePair()
	if err != nil {
		t.Fatal(err)
	}
	defer ports.Free(allocatedRTP, allocatedRTCP)

	var authorized *core.EventContext
	bus.Register(core.HookRegistration{
		Event: core.EventPublish,
		Mode:  core.HookSync,
		Handler: func(ctx *core.EventContext) error {
			authorized = ctx
			return errors.New("rejected")
		},
	})
	h := &handler{
		registry: NewDeviceRegistry(time.Minute, ""),
		sessions: NewSessionManager(),
		hub:      hub,
		bus:      bus,
		ports:    ports,
		prefix:   "gb28181",
	}
	req := newGBRequest(sip.INVITE, "device", "channel")
	req.Recipient.UriParams = sip.NewParams()
	req.Recipient.UriParams.Add("token", "query-token")
	req.AppendHeader(sip.NewHeader("Authorization", "Bearer bearer-token"))
	req.SetBody([]byte("v=0\r\nm=video 30000 RTP/AVP 96\r\n"))
	tx := &captureServerTransaction{}

	h.handleInvite(req, tx)

	if response := tx.lastResponse(); response == nil || response.StatusCode != 403 {
		t.Fatalf("rejected INVITE response = %#v, want 403", response)
	}
	if authorized == nil || authorized.Params["token"] != "query-token" {
		t.Fatalf("authorization context = %#v, want query token precedence", authorized)
	}
	if _, ok := hub.Find("gb28181/channel"); ok {
		t.Fatal("authorization rejection created a stream")
	}
	if got := len(h.sessions.All()); got != 0 {
		t.Fatalf("authorization rejection created %d sessions", got)
	}
}

func TestOutboundAPIsRejectAuthorizationBeforeResourceMutation(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		call func(*Module, http.ResponseWriter, *http.Request)
	}{
		{
			name: "live",
			call: func(module *Module, w http.ResponseWriter, r *http.Request) {
				module.apiPlay(w, r, "channel")
			},
		},
		{
			name: "playback",
			body: `{"start_time":"2026-08-24T10:00:00Z","end_time":"2026-08-24T10:05:00Z"}`,
			call: func(module *Module, w http.ResponseWriter, r *http.Request) {
				module.apiPlayback(w, r, "channel")
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			bus := core.NewEventBus()
			hub := core.NewStreamHub(config.StreamConfig{RingBufferSize: 16}, config.LimitsConfig{}, bus)
			ports, err := portalloc.New(42002, 42003)
			if err != nil {
				t.Fatal(err)
			}
			rtp, rtcp, err := ports.AllocatePair()
			if err != nil {
				t.Fatal(err)
			}
			defer ports.Free(rtp, rtcp)

			var authorized *core.EventContext
			bus.Register(core.HookRegistration{
				Event: core.EventPublish,
				Mode:  core.HookSync,
				Handler: func(ctx *core.EventContext) error {
					authorized = ctx
					return errors.New("rejected")
				},
			})
			registry := NewDeviceRegistry(time.Minute, "")
			registry.Register("device", "127.0.0.1:5060", "udp")
			registry.UpdateChannels("device", map[string]*Channel{
				"channel": {ChannelID: "channel", Status: "ON"},
			})
			h := &handler{
				registry: registry,
				sessions: NewSessionManager(),
				hub:      hub,
				bus:      bus,
				ports:    ports,
				prefix:   "gb28181",
			}
			module := &Module{
				registry: registry,
				sessions: h.sessions,
				handler:  h,
				invite:   &inviteClient{handler: h},
				playback: &playbackClient{handler: h},
			}
			req := httptest.NewRequest(http.MethodPost, "/api/v1/gb28181/channels/channel/"+test.name+"?token=query-token", strings.NewReader(test.body))
			req.Header.Set("Authorization", "Bearer management-token")
			rr := httptest.NewRecorder()

			test.call(module, rr, req)

			if rr.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401: %s", rr.Code, rr.Body.String())
			}
			if got := rr.Header().Get("WWW-Authenticate"); got != "Bearer" {
				t.Fatalf("WWW-Authenticate = %q, want Bearer", got)
			}
			if authorized == nil || authorized.Params["token"] != "query-token" {
				t.Fatalf("authorization context = %#v, want query token precedence", authorized)
			}
			if _, ok := hub.Find("gb28181/channel"); ok {
				t.Fatal("authorization rejection created a live stream")
			}
			if _, ok := hub.Find("gb28181/channel/playback"); ok {
				t.Fatal("authorization rejection created a playback stream")
			}
			if got := len(h.sessions.All()); got != 0 {
				t.Fatalf("authorization rejection created %d sessions", got)
			}
		})
	}
}
