package gb28181

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	sipmod "github.com/im-pingo/liveforge/module/sip"
	"github.com/im-pingo/liveforge/pkg/portalloc"
)

type failingACKDialog struct {
	response *sip.Response
	done     chan struct{}
	ackErr   error
	byeCalls atomic.Int32
	closed   atomic.Bool
}

func newFailingACKDialog(req *sip.Request) *failingACKDialog {
	done := make(chan struct{})
	close(done)
	resp := sip.NewResponseFromRequest(req, 200, "OK", []byte("v=0\r\nm=video 30000 RTP/AVP 96\r\n"))
	resp.AppendHeader(sip.NewHeader("To", "<sip:channel@127.0.0.1>;tag=accepted"))
	return &failingACKDialog{response: resp, done: done, ackErr: errors.New("ACK failed")}
}

func (d *failingACKDialog) Done() <-chan struct{}         { return d.done }
func (d *failingACKDialog) Response() *sip.Response       { return d.response }
func (d *failingACKDialog) SendACK(context.Context) error { return d.ackErr }
func (d *failingACKDialog) SendBYE(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("BYE used canceled setup context: %w", err)
	}
	d.byeCalls.Add(1)
	return nil
}
func (d *failingACKDialog) Close() { d.closed.Store(true) }

type successfulInviteDialog struct {
	response *sip.Response
	done     chan struct{}
	ackCalls atomic.Int32
	byeCalls atomic.Int32
	closed   atomic.Bool
}

func newSuccessfulInviteDialog(req *sip.Request) *successfulInviteDialog {
	done := make(chan struct{})
	close(done)
	resp := sip.NewResponseFromRequest(req, 200, "OK", []byte("v=0\r\nm=video 30000 RTP/AVP 96\r\n"))
	resp.AppendHeader(sip.NewHeader("To", "<sip:channel@127.0.0.1>;tag=accepted"))
	return &successfulInviteDialog{response: resp, done: done}
}

func (d *successfulInviteDialog) Done() <-chan struct{}         { return d.done }
func (d *successfulInviteDialog) Response() *sip.Response       { return d.response }
func (d *successfulInviteDialog) SendACK(context.Context) error { d.ackCalls.Add(1); return nil }
func (d *successfulInviteDialog) SendBYE(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	d.byeCalls.Add(1)
	return nil
}
func (d *successfulInviteDialog) Close() { d.closed.Store(true) }

type failingFinalResponseTransaction struct {
	*captureServerTransaction
}

func (t *failingFinalResponseTransaction) Respond(response *sip.Response) error {
	_ = t.captureServerTransaction.Respond(response)
	if response.StatusCode == 200 {
		return errors.New("final response write failed")
	}
	return nil
}

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

func TestInboundFinalResponseFailureRollsBackAcceptedSetup(t *testing.T) {
	bus := core.NewEventBus()
	hub := core.NewStreamHub(config.StreamConfig{RingBufferSize: 16}, config.LimitsConfig{}, bus)
	ports, err := portalloc.New(42130, 42131)
	if err != nil {
		t.Fatal(err)
	}
	sessions := NewSessionManager()
	h := &handler{registry: NewDeviceRegistry(time.Minute, ""), sessions: sessions, hub: hub, bus: bus, ports: ports, prefix: "gb28181"}
	var starts, stops atomic.Int32
	for event, counter := range map[core.EventType]*atomic.Int32{core.EventPublish: &starts, core.EventPublishStop: &stops} {
		bus.Register(core.HookRegistration{Event: event, Mode: core.HookAsync, Handler: func(*core.EventContext) error {
			counter.Add(1)
			return nil
		}})
	}
	req := newGBRequest(sip.INVITE, "device", "channel")
	req.SetBody([]byte("v=0\r\nm=video 30000 RTP/AVP 96\r\n"))
	tx := &failingFinalResponseTransaction{captureServerTransaction: &captureServerTransaction{}}

	h.handleInvite(req, tx)

	if got := len(sessions.All()); got != 0 {
		t.Fatalf("final-response failure left %d sessions", got)
	}
	if stream, ok := hub.Find("gb28181/channel"); ok && stream.Publisher() != nil {
		t.Fatal("final-response failure left publisher installed")
	}
	reused, err := NewRTPReceiver(42130, NewPublisher("reuse", nil))
	if err != nil {
		t.Fatalf("final-response failure left receiver socket open: %v", err)
	}
	reused.Close()
	rtpPort, rtcpPort, err := ports.AllocatePair()
	if err != nil || rtpPort != 42130 || rtcpPort != 42131 {
		t.Fatalf("final-response failure did not release allocator pair: %d/%d, %v", rtpPort, rtcpPort, err)
	}
	ports.Free(rtpPort, rtcpPort)
	time.Sleep(20 * time.Millisecond)
	if starts.Load() != 0 || stops.Load() != 0 {
		t.Fatalf("failed setup emitted lifecycle start/stop = %d/%d", starts.Load(), stops.Load())
	}
}

func TestOutboundACKFailureTerminatesDialogAndRollsBack(t *testing.T) {
	for _, test := range []struct {
		name string
		run  func(*handler, *Device, inviteSender) error
	}{
		{
			name: "live",
			run: func(h *handler, device *Device, send inviteSender) error {
				_, err := (&inviteClient{sipService: failingInviteService{}, handler: h, sendInvite: send}).invite(context.Background(), device, "channel", nil)
				return err
			},
		},
		{
			name: "playback",
			run: func(h *handler, device *Device, send inviteSender) error {
				_, err := (&playbackClient{sipService: failingInviteService{}, handler: h, sendInvite: send}).playback(
					context.Background(), device, "channel", time.Now(), time.Now().Add(time.Minute), nil,
				)
				return err
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			bus := core.NewEventBus()
			hub := core.NewStreamHub(config.StreamConfig{RingBufferSize: 16}, config.LimitsConfig{}, bus)
			ports, err := portalloc.New(42140, 42141)
			if err != nil {
				t.Fatal(err)
			}
			sessions := NewSessionManager()
			h := &handler{sessions: sessions, hub: hub, bus: bus, ports: ports, prefix: "gb28181"}
			device := &Device{DeviceID: "device", RemoteAddr: "127.0.0.1:5060", Transport: "udp"}
			var dialog *failingACKDialog
			send := func(_ context.Context, req *sip.Request) (inviteDialog, error) {
				dialog = newFailingACKDialog(req)
				return dialog, nil
			}
			var starts, stops atomic.Int32
			for event, counter := range map[core.EventType]*atomic.Int32{core.EventPublish: &starts, core.EventPublishStop: &stops} {
				bus.Register(core.HookRegistration{Event: event, Mode: core.HookAsync, Handler: func(*core.EventContext) error {
					counter.Add(1)
					return nil
				}})
			}

			err = test.run(h, device, send)
			if err == nil || !errors.Is(err, dialog.ackErr) {
				t.Fatalf("ACK failure error = %v, want wrapped %v", err, dialog.ackErr)
			}
			if dialog.byeCalls.Load() != 1 || !dialog.closed.Load() {
				t.Fatalf("accepted dialog cleanup: BYE calls=%d closed=%v", dialog.byeCalls.Load(), dialog.closed.Load())
			}
			if got := len(sessions.All()); got != 0 {
				t.Fatalf("ACK failure left %d sessions", got)
			}
			for _, key := range []string{"gb28181/channel", "gb28181/channel/playback"} {
				if stream, ok := hub.Find(key); ok && stream.Publisher() != nil {
					t.Fatalf("ACK failure left publisher on %s", key)
				}
			}
			reused, err := NewRTPReceiver(42140, NewPublisher("reuse", nil))
			if err != nil {
				t.Fatalf("ACK failure left receiver socket open: %v", err)
			}
			reused.Close()
			rtpPort, rtcpPort, err := ports.AllocatePair()
			if err != nil || rtpPort != 42140 || rtcpPort != 42141 {
				t.Fatalf("ACK failure did not release allocator pair: %d/%d, %v", rtpPort, rtcpPort, err)
			}
			ports.Free(rtpPort, rtcpPort)
			time.Sleep(20 * time.Millisecond)
			if starts.Load() != 0 || stops.Load() != 0 {
				t.Fatalf("failed setup emitted lifecycle start/stop = %d/%d", starts.Load(), stops.Load())
			}
		})
	}
}

func TestOutboundPublisherConflictTerminatesAcceptedDialogAndRollsBack(t *testing.T) {
	for _, test := range []struct {
		name      string
		streamKey string
		run       func(*handler, *Device, inviteSender) error
	}{
		{
			name:      "live",
			streamKey: "gb28181/channel",
			run: func(h *handler, device *Device, send inviteSender) error {
				_, err := (&inviteClient{sipService: failingInviteService{}, handler: h, sendInvite: send}).invite(context.Background(), device, "channel", nil)
				return err
			},
		},
		{
			name:      "playback",
			streamKey: "gb28181/channel/playback",
			run: func(h *handler, device *Device, send inviteSender) error {
				_, err := (&playbackClient{sipService: failingInviteService{}, handler: h, sendInvite: send}).playback(
					context.Background(), device, "channel", time.Now(), time.Now().Add(time.Minute), nil,
				)
				return err
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			bus := core.NewEventBus()
			hub := core.NewStreamHub(config.StreamConfig{RingBufferSize: 16}, config.LimitsConfig{}, bus)
			ports, err := portalloc.New(42180, 42181)
			if err != nil {
				t.Fatal(err)
			}
			stream, err := hub.GetOrCreate(test.streamKey)
			if err != nil {
				t.Fatal(err)
			}
			existing := NewPublisher("existing-generation", nil)
			if err := stream.SetPublisher(existing); err != nil {
				t.Fatal(err)
			}
			sessions := NewSessionManager()
			h := &handler{sessions: sessions, hub: hub, bus: bus, ports: ports, prefix: "gb28181"}
			device := &Device{DeviceID: "device", RemoteAddr: "127.0.0.1:5060", Transport: "udp"}
			var dialog *successfulInviteDialog
			send := func(_ context.Context, req *sip.Request) (inviteDialog, error) {
				dialog = newSuccessfulInviteDialog(req)
				return dialog, nil
			}
			var starts, stops atomic.Int32
			for event, counter := range map[core.EventType]*atomic.Int32{core.EventPublish: &starts, core.EventPublishStop: &stops} {
				bus.Register(core.HookRegistration{Event: event, Mode: core.HookAsync, Handler: func(*core.EventContext) error {
					counter.Add(1)
					return nil
				}})
			}

			err = test.run(h, device, send)
			if err == nil {
				t.Fatal("publisher conflict unexpectedly succeeded")
			}
			if dialog.ackCalls.Load() != 1 || dialog.byeCalls.Load() != 1 || !dialog.closed.Load() {
				t.Fatalf("accepted dialog cleanup: ACK=%d BYE=%d closed=%v", dialog.ackCalls.Load(), dialog.byeCalls.Load(), dialog.closed.Load())
			}
			if got := len(sessions.All()); got != 0 {
				t.Fatalf("publisher conflict left %d sessions", got)
			}
			if got := stream.Publisher(); got != existing {
				t.Fatalf("publisher conflict replaced existing publisher: got=%p want=%p", got, existing)
			}
			existing.mu.Lock()
			existingClosed := existing.closed
			existing.mu.Unlock()
			if existingClosed {
				t.Fatal("rollback closed the existing publisher")
			}
			reused, err := NewRTPReceiver(42180, NewPublisher("reuse", nil))
			if err != nil {
				t.Fatalf("publisher conflict left receiver socket open: %v", err)
			}
			reused.Close()
			rtpPort, rtcpPort, err := ports.AllocatePair()
			if err != nil || rtpPort != 42180 || rtcpPort != 42181 {
				t.Fatalf("publisher conflict did not release allocator pair: %d/%d, %v", rtpPort, rtcpPort, err)
			}
			ports.Free(rtpPort, rtcpPort)
			time.Sleep(20 * time.Millisecond)
			if starts.Load() != 0 || stops.Load() != 0 {
				t.Fatalf("publisher conflict emitted lifecycle start/stop = %d/%d", starts.Load(), stops.Load())
			}
		})
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

func TestMediaSessionPublishLifecycleIsAtomicWithClose(t *testing.T) {
	session := &MediaSession{State: SessionStateStreaming}
	emitEntered := make(chan struct{})
	releaseEmit := make(chan struct{})
	startResult := make(chan bool, 1)
	go func() {
		startResult <- session.startPublishLifecycle(func() {
			close(emitEntered)
			<-releaseEmit
		})
	}()
	<-emitEntered

	closeResult := make(chan bool, 1)
	go func() {
		closeResult <- session.Close()
	}()
	select {
	case <-closeResult:
		t.Fatal("session close interleaved between publish mark and lifecycle enqueue")
	case <-time.After(20 * time.Millisecond):
	}

	close(releaseEmit)
	if !<-startResult {
		t.Fatal("publish lifecycle start was rejected")
	}
	if !<-closeResult {
		t.Fatal("session close did not own cleanup")
	}
	if !session.publishLifecycleStarted() {
		t.Fatal("publish lifecycle was not retained in the close snapshot")
	}
}

func TestMediaSessionPublishLifecycleRejectsStartAfterClose(t *testing.T) {
	session := &MediaSession{State: SessionStateStreaming}
	if !session.Close() {
		t.Fatal("session close did not own cleanup")
	}
	emitted := false
	if session.startPublishLifecycle(func() { emitted = true }) {
		t.Fatal("publish lifecycle started after close")
	}
	if emitted {
		t.Fatal("publish lifecycle callback ran after close")
	}
}

func TestInboundPublishStopWaitsForBlockedStart(t *testing.T) {
	bus := core.NewEventBus()
	hub := core.NewStreamHub(config.StreamConfig{RingBufferSize: 16}, config.LimitsConfig{}, bus)
	ports, err := portalloc.New(42160, 42161)
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
	startEntered, releaseStart, stopRan := registerBlockedGBPublishLifecycle(bus)
	req := newGBRequest(sip.INVITE, "device", "channel")
	req.SetBody([]byte("v=0\r\nm=video 30000 RTP/AVP 96\r\n"))
	tx := &captureServerTransaction{}

	h.handleInvite(req, tx)
	if response := tx.lastResponse(); response == nil || response.StatusCode != 200 {
		t.Fatalf("inbound response = %#v, want 200", response)
	}
	start := receiveGBLifecycle(t, startEntered, "inbound start")
	all := sessions.All()
	if len(all) != 1 || !h.closeSession(all[0], "127.0.0.1:5060") {
		t.Fatalf("inbound active sessions = %d, want one closable session", len(all))
	}
	assertGBStopBlocked(t, stopRan, "inbound")
	close(releaseStart)
	stop := receiveGBLifecycle(t, stopRan, "inbound stop")
	if stop.PublisherID != start.PublisherID || start.PublisherID == "" {
		t.Fatalf("inbound publisher generation = %q then %q", start.PublisherID, stop.PublisherID)
	}
}

func TestOutboundPublishStopWaitsForBlockedStart(t *testing.T) {
	for _, test := range []struct {
		name string
		run  func(*handler, *Device, inviteSender) (*MediaSession, error)
	}{
		{
			name: "live",
			run: func(h *handler, device *Device, send inviteSender) (*MediaSession, error) {
				return (&inviteClient{sipService: failingInviteService{}, handler: h, sendInvite: send}).invite(context.Background(), device, "channel", nil)
			},
		},
		{
			name: "playback",
			run: func(h *handler, device *Device, send inviteSender) (*MediaSession, error) {
				return (&playbackClient{sipService: failingInviteService{}, handler: h, sendInvite: send}).playback(
					context.Background(), device, "channel", time.Now(), time.Now().Add(time.Minute), nil,
				)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			bus := core.NewEventBus()
			hub := core.NewStreamHub(config.StreamConfig{RingBufferSize: 16}, config.LimitsConfig{}, bus)
			ports, err := portalloc.New(42170, 42171)
			if err != nil {
				t.Fatal(err)
			}
			h := &handler{sessions: NewSessionManager(), hub: hub, bus: bus, ports: ports, prefix: "gb28181"}
			device := &Device{DeviceID: "device", RemoteAddr: "127.0.0.1:5060", Transport: "udp"}
			startEntered, releaseStart, stopRan := registerBlockedGBPublishLifecycle(bus)
			session, err := test.run(h, device, func(_ context.Context, req *sip.Request) (inviteDialog, error) {
				return newSuccessfulInviteDialog(req), nil
			})
			if err != nil {
				t.Fatal(err)
			}
			start := receiveGBLifecycle(t, startEntered, test.name+" start")
			if !h.closeSession(session, "") {
				t.Fatal("outbound session cleanup did not run")
			}
			assertGBStopBlocked(t, stopRan, test.name)
			close(releaseStart)
			stop := receiveGBLifecycle(t, stopRan, test.name+" stop")
			if stop.PublisherID != start.PublisherID || start.PublisherID == "" {
				t.Fatalf("%s publisher generation = %q then %q", test.name, start.PublisherID, stop.PublisherID)
			}
		})
	}
}

func registerBlockedGBPublishLifecycle(bus *core.EventBus) (chan *core.EventContext, chan struct{}, chan *core.EventContext) {
	startEntered := make(chan *core.EventContext, 1)
	releaseStart := make(chan struct{})
	stopRan := make(chan *core.EventContext, 1)
	bus.Register(core.HookRegistration{
		Event: core.EventPublish,
		Mode:  core.HookAsync,
		Handler: func(ctx *core.EventContext) error {
			startEntered <- ctx
			<-releaseStart
			return nil
		},
	})
	bus.Register(core.HookRegistration{
		Event: core.EventPublishStop,
		Mode:  core.HookAsync,
		Handler: func(ctx *core.EventContext) error {
			stopRan <- ctx
			return nil
		},
	})
	return startEntered, releaseStart, stopRan
}

func receiveGBLifecycle(t *testing.T, events <-chan *core.EventContext, name string) *core.EventContext {
	t.Helper()
	select {
	case ctx := <-events:
		return ctx
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for GB28181 %s", name)
		return nil
	}
}

func assertGBStopBlocked(t *testing.T, stops <-chan *core.EventContext, name string) {
	t.Helper()
	select {
	case <-stops:
		t.Fatalf("GB28181 %s stop overtook blocked start", name)
	case <-time.After(20 * time.Millisecond):
	}
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

func TestHandlerCleanupRemovesOnlyMatchingSession(t *testing.T) {
	bus := core.NewEventBus()
	hub := core.NewStreamHub(config.StreamConfig{RingBufferSize: 16}, config.LimitsConfig{}, bus)
	stream, err := hub.GetOrCreate("gb28181/channel")
	if err != nil {
		t.Fatal(err)
	}
	oldPublisher := NewPublisher("old", nil)
	if err := stream.SetPublisher(oldPublisher); err != nil {
		t.Fatal(err)
	}
	old := &MediaSession{ID: "reused-call-id", StreamKey: "gb28181/channel", Publisher: oldPublisher, Stream: stream, State: SessionStateStreaming}
	replacement := &MediaSession{ID: old.ID, StreamKey: old.StreamKey, State: SessionStateInviting}
	sessions := NewSessionManager()
	sessions.Add(old)
	sessions.Add(replacement)
	h := &handler{sessions: sessions, hub: hub, bus: bus}

	if !h.closeSession(old, "") {
		t.Fatal("old session cleanup did not run")
	}
	if got := sessions.Get(old.ID); got != replacement {
		t.Fatalf("old cleanup removed replacement session: got %p want %p", got, replacement)
	}
}

func TestConcurrentSessionCleanupAndSnapshotReads(t *testing.T) {
	bus := core.NewEventBus()
	hub := core.NewStreamHub(config.StreamConfig{RingBufferSize: 16}, config.LimitsConfig{}, bus)
	ports, err := portalloc.New(42150, 42151)
	if err != nil {
		t.Fatal(err)
	}
	rtpPort, _, err := ports.AllocatePair()
	if err != nil {
		t.Fatal(err)
	}
	stream, _ := hub.GetOrCreate("gb28181/concurrent")
	pub := NewPublisher("concurrent-generation", nil)
	if err := stream.SetPublisher(pub); err != nil {
		t.Fatal(err)
	}
	session := &MediaSession{ID: "concurrent", DeviceID: "device", ChannelID: "channel", StreamKey: "gb28181/concurrent", LocalPort: rtpPort, State: SessionStateStreaming, Publisher: pub, Stream: stream}
	if !session.MarkPublished() {
		t.Fatal("publish lifecycle mark failed")
	}
	sessions := NewSessionManager()
	sessions.Add(session)
	h := &handler{sessions: sessions, hub: hub, bus: bus, ports: ports}
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for range 100 {
				_ = sessions.GetByChannel("channel")
				_ = session.Snapshot()
			}
		}()
		go func() {
			defer wg.Done()
			h.closeSession(session, "")
		}()
	}
	wg.Wait()
	if got := len(sessions.GetByChannel("channel")); got != 0 {
		t.Fatalf("closed session remained in channel snapshot: %d", got)
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
