package gb28181

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	sipmod "github.com/im-pingo/liveforge/module/sip"
	"github.com/im-pingo/liveforge/pkg/portalloc"
)

type failingInviteService struct{}

func (failingInviteService) OnRegister(sipmod.RegisterHandler)   {}
func (failingInviteService) OnInvite(sipmod.InviteHandler)       {}
func (failingInviteService) OnBye(sipmod.ByeHandler)             {}
func (failingInviteService) OnMessage(sipmod.MessageHandler)     {}
func (failingInviteService) OnSubscribe(sipmod.SubscribeHandler) {}
func (failingInviteService) OnNotify(sipmod.NotifyHandler)       {}
func (failingInviteService) SendRequest(context.Context, *sip.Request) (*sip.Response, error) {
	return nil, errors.New("send failed")
}
func (failingInviteService) SendInvite(context.Context, *sip.Request) (*sipmod.InviteTransaction, error) {
	return nil, errors.New("send failed")
}
func (failingInviteService) LocalAddr() string { return "127.0.0.1:5060" }
func (failingInviteService) ServerID() string  { return "34020000002000000001" }
func (failingInviteService) Domain() string    { return "3402000000" }

func TestInboundReceiverFailureRollsBackAllResources(t *testing.T) {
	bus := core.NewEventBus()
	hub := core.NewStreamHub(config.StreamConfig{RingBufferSize: 16}, config.LimitsConfig{}, bus)
	ports, err := portalloc.New(42110, 42111)
	if err != nil {
		t.Fatal(err)
	}
	sessions := NewSessionManager()
	h := &handler{
		registry: NewDeviceRegistry(time.Minute, ""),
		sessions: sessions,
		hub:      hub,
		bus:      bus,
		ports:    ports,
		prefix:   "gb28181",
	}

	original := newRTPReceiver
	newRTPReceiver = func(int, *Publisher) (*RTPReceiver, error) {
		return nil, errors.New("receiver failed")
	}
	t.Cleanup(func() { newRTPReceiver = original })

	stops := make(chan *core.EventContext, 1)
	bus.Register(core.HookRegistration{
		Event: core.EventPublishStop,
		Mode:  core.HookAsync,
		Handler: func(ctx *core.EventContext) error {
			stops <- ctx
			return nil
		},
	})
	req := newGBRequest("INVITE", "device", "channel")
	req.SetBody([]byte("v=0\r\nm=video 30000 RTP/AVP 96\r\n"))
	tx := &captureServerTransaction{}

	h.handleInvite(req, tx)

	if response := tx.lastResponse(); response == nil || response.StatusCode != 500 {
		t.Fatalf("receiver failure response = %#v, want 500", response)
	}
	if got := len(sessions.All()); got != 0 {
		t.Fatalf("receiver failure left %d sessions", got)
	}
	if stream, ok := hub.Find("gb28181/channel"); ok && stream.Publisher() != nil {
		t.Fatal("receiver failure left publisher installed")
	}
	rtpPort, rtcpPort, err := ports.AllocatePair()
	if err != nil {
		t.Fatalf("receiver failure leaked RTP ports: %v", err)
	}
	ports.Free(rtpPort, rtcpPort)
	select {
	case stop := <-stops:
		t.Fatalf("receiver setup rollback emitted unmatched stop: %#v", stop)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestOutboundSendFailureClosesOwnedReceiverAndRollsBack(t *testing.T) {
	for _, test := range []struct {
		name string
		run  func(*handler, *Device) error
	}{
		{
			name: "live",
			run: func(h *handler, device *Device) error {
				_, err := (&inviteClient{sipService: failingInviteService{}, handler: h}).invite(context.Background(), device, "channel", nil)
				return err
			},
		},
		{
			name: "playback",
			run: func(h *handler, device *Device) error {
				_, err := (&playbackClient{sipService: failingInviteService{}, handler: h}).playback(
					context.Background(), device, "channel", time.Now(), time.Now().Add(time.Minute), nil,
				)
				return err
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			bus := core.NewEventBus()
			hub := core.NewStreamHub(config.StreamConfig{RingBufferSize: 16}, config.LimitsConfig{}, bus)
			ports, err := portalloc.New(42120, 42121)
			if err != nil {
				t.Fatal(err)
			}
			sessions := NewSessionManager()
			h := &handler{sessions: sessions, hub: hub, bus: bus, ports: ports, prefix: "gb28181"}
			device := &Device{DeviceID: "device", RemoteAddr: "127.0.0.1:5060", Transport: "udp"}

			original := newRTPReceiver
			created := 0
			boundPort := 0
			newRTPReceiver = func(port int, publisher *Publisher) (*RTPReceiver, error) {
				created++
				boundPort = port
				return NewRTPReceiver(port, publisher)
			}
			t.Cleanup(func() { newRTPReceiver = original })

			if err := test.run(h, device); err == nil {
				t.Fatal("outbound setup unexpectedly succeeded")
			}
			if created != 1 {
				t.Fatalf("receiver creations = %d, want 1 before SIP send", created)
			}
			if got := len(sessions.All()); got != 0 {
				t.Fatalf("send failure left %d sessions", got)
			}
			for _, key := range []string{"gb28181/channel", "gb28181/channel/playback"} {
				if stream, ok := hub.Find(key); ok && stream.Publisher() != nil {
					t.Fatalf("send failure left publisher on %s", key)
				}
			}
			reused, err := NewRTPReceiver(boundPort, NewPublisher("reuse", nil))
			if err != nil {
				t.Fatalf("send failure left receiver socket open: %v", err)
			}
			reused.Close()
			rtpPort, rtcpPort, err := ports.AllocatePair()
			if err != nil {
				t.Fatalf("send failure leaked port allocation: %v", err)
			}
			ports.Free(rtpPort, rtcpPort)
		})
	}
}

func TestMediaSessionCloseOwnsReceiverAndAllowsPortReuse(t *testing.T) {
	pub := NewPublisher("publisher", nil)
	receiver, err := NewRTPReceiver(0, pub)
	if err != nil {
		t.Fatal(err)
	}
	port := receiver.LocalPort()
	session := &MediaSession{
		State:     SessionStateStreaming,
		Publisher: pub,
		Receiver:  receiver,
	}

	if !session.Close() {
		t.Fatal("first close did not own session termination")
	}
	if session.Close() {
		t.Fatal("second close owned session termination")
	}

	reused, err := NewRTPReceiver(port, NewPublisher("replacement", nil))
	if err != nil {
		t.Fatalf("receiver port was not released by session close: %v", err)
	}
	reused.Close()
}

func TestHandlerCloseSessionEmitsOneGenerationTaggedStop(t *testing.T) {
	bus := core.NewEventBus()
	hub := core.NewStreamHub(config.StreamConfig{RingBufferSize: 16}, config.LimitsConfig{}, bus)
	ports, err := portalloc.New(42100, 42101)
	if err != nil {
		t.Fatal(err)
	}
	rtpPort, rtcpPort, err := ports.AllocatePair()
	if err != nil {
		t.Fatal(err)
	}
	stream, err := hub.GetOrCreate("gb28181/channel")
	if err != nil {
		t.Fatal(err)
	}
	pub := NewPublisher("gb28181-channel-generation-9", nil)
	if err := stream.SetPublisher(pub); err != nil {
		t.Fatal(err)
	}
	sessions := NewSessionManager()
	session := &MediaSession{
		ID:        "call-9",
		DeviceID:  "device",
		ChannelID: "channel",
		StreamKey: "gb28181/channel",
		LocalPort: rtpPort,
		State:     SessionStateStreaming,
		Publisher: pub,
		Stream:    stream,
	}
	if !session.MarkPublished() {
		t.Fatal("active session refused publish lifecycle mark")
	}
	sessions.Add(session)
	h := &handler{sessions: sessions, bus: bus, ports: ports}

	stops := make(chan *core.EventContext, 2)
	bus.Register(core.HookRegistration{
		Event: core.EventPublishStop,
		Mode:  core.HookAsync,
		Handler: func(ctx *core.EventContext) error {
			stops <- ctx
			return nil
		},
	})

	if !h.closeSession(session, "127.0.0.1:5060") {
		t.Fatal("first handler cleanup did not close session")
	}
	if h.closeSession(session, "127.0.0.1:5060") {
		t.Fatal("repeated handler cleanup closed session twice")
	}

	select {
	case stop := <-stops:
		if stop.PublisherID != pub.ID() {
			t.Fatalf("stop publisher ID = %q, want %q", stop.PublisherID, pub.ID())
		}
	case <-time.After(time.Second):
		t.Fatal("publish stop event not emitted")
	}
	select {
	case stop := <-stops:
		t.Fatalf("duplicate publish stop event: %#v", stop)
	case <-time.After(50 * time.Millisecond):
	}
	if sessions.Get(session.ID) != nil {
		t.Fatal("closed session remains registered")
	}
	if stream.Publisher() != nil {
		t.Fatal("closed publisher remains installed")
	}
	reusedRTP, reusedRTCP, err := ports.AllocatePair()
	if err != nil {
		t.Fatalf("closed session ports were not freed: %v", err)
	}
	if reusedRTP != rtpPort || reusedRTCP != rtcpPort {
		t.Fatalf("reused ports = %d/%d, want %d/%d", reusedRTP, reusedRTCP, rtpPort, rtcpPort)
	}
}

func TestPublisherIDsAreProcessUnique(t *testing.T) {
	seen := make(map[string]struct{})
	for i := 0; i < 1000; i++ {
		id := newPublisherID("live", "channel")
		if _, exists := seen[id]; exists {
			t.Fatalf("duplicate publisher ID %q", id)
		}
		seen[id] = struct{}{}
	}
}
