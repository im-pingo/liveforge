package sip

import (
	"context"
	"errors"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

type offeredResponseClientTx struct {
	responses chan *sip.Response
	done      chan struct{}
	once      sync.Once
}

func newOfferedResponseClientTx() *offeredResponseClientTx {
	return &offeredResponseClientTx{
		responses: make(chan *sip.Response),
		done:      make(chan struct{}),
	}
}

func (tx *offeredResponseClientTx) Responses() <-chan *sip.Response { return tx.responses }
func (tx *offeredResponseClientTx) Done() <-chan struct{}           { return tx.done }
func (tx *offeredResponseClientTx) Err() error                      { return nil }
func (tx *offeredResponseClientTx) Terminate()                      { tx.once.Do(func() { close(tx.done) }) }
func (tx *offeredResponseClientTx) OnTerminate(sip.FnTxTerminate) bool {
	return true
}
func (tx *offeredResponseClientTx) OnRetransmission(sip.FnTxResponse) bool {
	return false
}

type clientTxRequesterFunc func(context.Context, *sip.Request) (sip.ClientTransaction, error)

func (f clientTxRequesterFunc) Request(ctx context.Context, req *sip.Request) (sip.ClientTransaction, error) {
	return f(ctx, req)
}

type blockingMessageConnection struct {
	net.Conn
	writeStarted chan struct{}
	writeDone    chan struct{}
	startOnce    sync.Once
}

func (c *blockingMessageConnection) WriteMsg(msg sip.Message) error {
	defer close(c.writeDone)
	if c.writeStarted != nil {
		c.startOnce.Do(func() { close(c.writeStarted) })
	}
	_, err := c.Write([]byte(msg.String()))
	return err
}
func (c *blockingMessageConnection) Ref(int) int            { return 1 }
func (c *blockingMessageConnection) TryClose() (int, error) { return 1, nil }

type failingDeadlineConnection struct {
	*blockingMessageConnection
}

func (c *failingDeadlineConnection) SetWriteDeadline(time.Time) error {
	return errors.New("test write deadline failure")
}

func TestWriteMessageWithContextBoundsBlockingWriteAndJoinsWriter(t *testing.T) {
	writeSide, blockedSide := net.Pipe()
	t.Cleanup(func() {
		_ = writeSide.Close()
		_ = blockedSide.Close()
	})
	conn := &blockingMessageConnection{Conn: writeSide, writeDone: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	err := writeMessageWithContext(ctx, conn, makeTestRequest(sip.ACK))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("writeMessageWithContext error = %v, want context.DeadlineExceeded", err)
	}
	select {
	case <-conn.writeDone:
	default:
		t.Fatal("writeMessageWithContext returned before the blocked writer terminated")
	}
}

func TestWriteMessageWithContextClosesBlockedConnectionWhenDeadlineInterruptFails(t *testing.T) {
	writeSide, blockedSide := net.Pipe()
	t.Cleanup(func() {
		_ = writeSide.Close()
		_ = blockedSide.Close()
	})
	base := &blockingMessageConnection{
		Conn:         writeSide,
		writeStarted: make(chan struct{}),
		writeDone:    make(chan struct{}),
	}
	conn := &failingDeadlineConnection{blockingMessageConnection: base}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- writeMessageWithContext(ctx, conn, makeTestRequest(sip.ACK))
	}()
	<-base.writeStarted
	cancel()

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("writeMessageWithContext error = %v, want context.Canceled", err)
		}
	case <-time.After(100 * time.Millisecond):
		_ = writeSide.Close()
		<-result
		t.Fatal("write remained blocked after cancellation deadline interrupt failed")
	}
	select {
	case <-base.writeDone:
	default:
		t.Fatal("writeMessageWithContext returned before the fallback close stopped the writer")
	}
}

// mockServerTx is a minimal mock implementing sip.ServerTransaction.
// It captures responses passed to Respond for later assertions.
type mockServerTx struct {
	mu        sync.Mutex
	responses []*sip.Response
}

func newMockServerTx() *mockServerTx {
	return &mockServerTx{}
}

func (m *mockServerTx) Respond(res *sip.Response) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.responses = append(m.responses, res)
	return nil
}

func (m *mockServerTx) Acks() <-chan *sip.Request {
	ch := make(chan *sip.Request)
	return ch
}

func (m *mockServerTx) OnCancel(f sip.FnTxCancel) bool {
	return false
}

func (m *mockServerTx) Terminate() {}

func (m *mockServerTx) OnTerminate(f sip.FnTxTerminate) bool {
	return false
}

func (m *mockServerTx) Done() <-chan struct{} {
	ch := make(chan struct{})
	return ch
}

func (m *mockServerTx) Err() error {
	return nil
}

// lastResponse returns the most recent response captured by Respond.
func (m *mockServerTx) lastResponse() *sip.Response {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.responses) == 0 {
		return nil
	}
	return m.responses[len(m.responses)-1]
}

// responseCount returns how many times Respond was called.
func (m *mockServerTx) responseCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.responses)
}

// makeTestRequest creates a minimal SIP request for the given method.
func makeTestRequest(method sip.RequestMethod) *sip.Request {
	return sip.NewRequest(method, sip.Uri{Host: "localhost", Port: 5060})
}

func TestSendInviteRetainsOfferedFinalResponseWhenCallerCanceled(t *testing.T) {
	previousProcs := runtime.GOMAXPROCS(1)
	t.Cleanup(func() { runtime.GOMAXPROCS(previousProcs) })

	ua, err := sipgo.NewUA()
	if err != nil {
		t.Fatalf("NewUA: %v", err)
	}
	t.Cleanup(func() { _ = ua.Close() })
	client, err := sipgo.NewClient(ua)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	service := newService()
	service.client = &sipClient{client: client}

	const attempts = 64
	lost := 0
	for range attempts {
		clientTx := newOfferedResponseClientTx()
		client.TxRequester = clientTxRequesterFunc(func(context.Context, *sip.Request) (sip.ClientTransaction, error) {
			return clientTx, nil
		})
		offerStarted := make(chan struct{})
		offerDone := make(chan struct{})
		go func() {
			close(offerStarted)
			select {
			case clientTx.responses <- sip.NewResponse(200, "OK"):
			case <-clientTx.done:
			}
			close(offerDone)
		}()
		<-offerStarted

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		inviteTx, err := service.SendInvite(ctx, makeTestRequest(sip.INVITE))
		if err != nil {
			t.Fatalf("SendInvite: %v", err)
		}
		select {
		case <-inviteTx.Done():
		case <-time.After(time.Second):
			t.Fatal("InviteTransaction did not finish after final response or cancellation")
		}
		<-offerDone
		if inviteTx.Response() == nil {
			lost++
		}
	}

	if lost != 0 {
		t.Fatalf("lost %d of %d final responses offered before cancellation wait", lost, attempts)
	}
}

func TestSendInviteCollectorStopsWhenClientTransactionTerminates(t *testing.T) {
	ua, err := sipgo.NewUA()
	if err != nil {
		t.Fatalf("NewUA: %v", err)
	}
	t.Cleanup(func() { _ = ua.Close() })
	client, err := sipgo.NewClient(ua)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	clientTx := newOfferedResponseClientTx()
	client.TxRequester = clientTxRequesterFunc(func(context.Context, *sip.Request) (sip.ClientTransaction, error) {
		return clientTx, nil
	})
	service := newService()
	service.client = &sipClient{client: client}

	inviteTx, err := service.SendInvite(context.Background(), makeTestRequest(sip.INVITE))
	if err != nil {
		t.Fatalf("SendInvite: %v", err)
	}
	clientTx.Terminate()

	select {
	case <-inviteTx.Done():
	case <-time.After(time.Second):
		t.Fatal("InviteTransaction collector remained blocked after client transaction termination")
	}
}

// ---------------------------------------------------------------------------
// Handler Registration
// ---------------------------------------------------------------------------

func TestOnRegisterRegistersHandler(t *testing.T) {
	s := newService()
	called := false
	s.OnRegister(func(req *sip.Request, tx sip.ServerTransaction) {
		called = true
	})

	s.mu.RLock()
	count := len(s.registerHandlers)
	s.mu.RUnlock()

	if count != 1 {
		t.Fatalf("expected 1 register handler, got %d", count)
	}

	tx := newMockServerTx()
	s.dispatchRegister(makeTestRequest(sip.REGISTER), tx)
	if !called {
		t.Fatal("register handler was not called")
	}
}

func TestOnInviteRegistersHandler(t *testing.T) {
	s := newService()
	called := false
	s.OnInvite(func(req *sip.Request, tx sip.ServerTransaction) {
		called = true
	})

	s.mu.RLock()
	count := len(s.inviteHandlers)
	s.mu.RUnlock()

	if count != 1 {
		t.Fatalf("expected 1 invite handler, got %d", count)
	}

	tx := newMockServerTx()
	s.dispatchInvite(makeTestRequest(sip.INVITE), tx)
	if !called {
		t.Fatal("invite handler was not called")
	}
}

func TestOnByeRegistersHandler(t *testing.T) {
	s := newService()
	called := false
	s.OnBye(func(req *sip.Request, tx sip.ServerTransaction) {
		called = true
	})

	s.mu.RLock()
	count := len(s.byeHandlers)
	s.mu.RUnlock()

	if count != 1 {
		t.Fatalf("expected 1 bye handler, got %d", count)
	}

	tx := newMockServerTx()
	s.dispatchBye(makeTestRequest(sip.BYE), tx)
	if !called {
		t.Fatal("bye handler was not called")
	}
}

func TestOnMessageRegistersHandler(t *testing.T) {
	s := newService()
	called := false
	s.OnMessage(func(req *sip.Request, tx sip.ServerTransaction) {
		called = true
	})

	s.mu.RLock()
	count := len(s.messageHandlers)
	s.mu.RUnlock()

	if count != 1 {
		t.Fatalf("expected 1 message handler, got %d", count)
	}

	tx := newMockServerTx()
	s.dispatchMessage(makeTestRequest(sip.MESSAGE), tx)
	if !called {
		t.Fatal("message handler was not called")
	}
}

func TestOnSubscribeRegistersHandler(t *testing.T) {
	s := newService()
	called := false
	s.OnSubscribe(func(req *sip.Request, tx sip.ServerTransaction) {
		called = true
	})

	s.mu.RLock()
	count := len(s.subscribeHandlers)
	s.mu.RUnlock()

	if count != 1 {
		t.Fatalf("expected 1 subscribe handler, got %d", count)
	}

	tx := newMockServerTx()
	s.dispatchSubscribe(makeTestRequest(sip.SUBSCRIBE), tx)
	if !called {
		t.Fatal("subscribe handler was not called")
	}
}

func TestOnNotifyRegistersHandler(t *testing.T) {
	s := newService()
	called := false
	s.OnNotify(func(req *sip.Request, tx sip.ServerTransaction) {
		called = true
	})

	s.mu.RLock()
	count := len(s.notifyHandlers)
	s.mu.RUnlock()

	if count != 1 {
		t.Fatalf("expected 1 notify handler, got %d", count)
	}

	tx := newMockServerTx()
	s.dispatchNotify(makeTestRequest(sip.NOTIFY), tx)
	if !called {
		t.Fatal("notify handler was not called")
	}
}

// ---------------------------------------------------------------------------
// Dispatch Without Handlers (default responses)
// ---------------------------------------------------------------------------

func TestDispatchWithoutHandlers(t *testing.T) {
	tests := []struct {
		name       string
		method     sip.RequestMethod
		dispatch   func(s *service, req *sip.Request, tx sip.ServerTransaction)
		wantStatus int
		wantReason string
	}{
		{
			name:       "REGISTER without handler returns 405",
			method:     sip.REGISTER,
			dispatch:   func(s *service, req *sip.Request, tx sip.ServerTransaction) { s.dispatchRegister(req, tx) },
			wantStatus: 405,
			wantReason: "Method Not Allowed",
		},
		{
			name:       "INVITE without handler returns 405",
			method:     sip.INVITE,
			dispatch:   func(s *service, req *sip.Request, tx sip.ServerTransaction) { s.dispatchInvite(req, tx) },
			wantStatus: 405,
			wantReason: "Method Not Allowed",
		},
		{
			name:       "BYE without handler returns 200",
			method:     sip.BYE,
			dispatch:   func(s *service, req *sip.Request, tx sip.ServerTransaction) { s.dispatchBye(req, tx) },
			wantStatus: 200,
			wantReason: "OK",
		},
		{
			name:       "MESSAGE without handler returns 200",
			method:     sip.MESSAGE,
			dispatch:   func(s *service, req *sip.Request, tx sip.ServerTransaction) { s.dispatchMessage(req, tx) },
			wantStatus: 200,
			wantReason: "OK",
		},
		{
			name:       "SUBSCRIBE without handler returns 405",
			method:     sip.SUBSCRIBE,
			dispatch:   func(s *service, req *sip.Request, tx sip.ServerTransaction) { s.dispatchSubscribe(req, tx) },
			wantStatus: 405,
			wantReason: "Method Not Allowed",
		},
		{
			name:       "NOTIFY without handler returns 200",
			method:     sip.NOTIFY,
			dispatch:   func(s *service, req *sip.Request, tx sip.ServerTransaction) { s.dispatchNotify(req, tx) },
			wantStatus: 200,
			wantReason: "OK",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newService()
			tx := newMockServerTx()
			req := makeTestRequest(tc.method)

			tc.dispatch(s, req, tx)

			resp := tx.lastResponse()
			if resp == nil {
				t.Fatal("expected a response, got nil")
			}
			if resp.StatusCode != tc.wantStatus {
				t.Errorf("status code = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			if resp.Reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", resp.Reason, tc.wantReason)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Dispatch With Handlers
// ---------------------------------------------------------------------------

func TestDispatchWithHandlers(t *testing.T) {
	tests := []struct {
		name     string
		method   sip.RequestMethod
		register func(s *service, called *bool)
		dispatch func(s *service, req *sip.Request, tx sip.ServerTransaction)
	}{
		{
			name:   "REGISTER dispatch calls handler",
			method: sip.REGISTER,
			register: func(s *service, called *bool) {
				s.OnRegister(func(req *sip.Request, tx sip.ServerTransaction) { *called = true })
			},
			dispatch: func(s *service, req *sip.Request, tx sip.ServerTransaction) { s.dispatchRegister(req, tx) },
		},
		{
			name:   "INVITE dispatch calls handler",
			method: sip.INVITE,
			register: func(s *service, called *bool) {
				s.OnInvite(func(req *sip.Request, tx sip.ServerTransaction) { *called = true })
			},
			dispatch: func(s *service, req *sip.Request, tx sip.ServerTransaction) { s.dispatchInvite(req, tx) },
		},
		{
			name:   "BYE dispatch calls handler",
			method: sip.BYE,
			register: func(s *service, called *bool) {
				s.OnBye(func(req *sip.Request, tx sip.ServerTransaction) { *called = true })
			},
			dispatch: func(s *service, req *sip.Request, tx sip.ServerTransaction) { s.dispatchBye(req, tx) },
		},
		{
			name:   "MESSAGE dispatch calls handler",
			method: sip.MESSAGE,
			register: func(s *service, called *bool) {
				s.OnMessage(func(req *sip.Request, tx sip.ServerTransaction) { *called = true })
			},
			dispatch: func(s *service, req *sip.Request, tx sip.ServerTransaction) { s.dispatchMessage(req, tx) },
		},
		{
			name:   "SUBSCRIBE dispatch calls handler",
			method: sip.SUBSCRIBE,
			register: func(s *service, called *bool) {
				s.OnSubscribe(func(req *sip.Request, tx sip.ServerTransaction) { *called = true })
			},
			dispatch: func(s *service, req *sip.Request, tx sip.ServerTransaction) { s.dispatchSubscribe(req, tx) },
		},
		{
			name:   "NOTIFY dispatch calls handler",
			method: sip.NOTIFY,
			register: func(s *service, called *bool) {
				s.OnNotify(func(req *sip.Request, tx sip.ServerTransaction) { *called = true })
			},
			dispatch: func(s *service, req *sip.Request, tx sip.ServerTransaction) { s.dispatchNotify(req, tx) },
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newService()
			called := false
			tc.register(s, &called)

			tx := newMockServerTx()
			req := makeTestRequest(tc.method)
			tc.dispatch(s, req, tx)

			if !called {
				t.Fatal("handler was not called")
			}
			// When a handler is registered, the service should not send a default response.
			if tx.responseCount() != 0 {
				t.Errorf("expected no auto-response when handler is registered, got %d responses", tx.responseCount())
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Multiple Handlers
// ---------------------------------------------------------------------------

func TestMultipleHandlersAllCalled(t *testing.T) {
	tests := []struct {
		name     string
		method   sip.RequestMethod
		register func(s *service, counters []*int32)
		dispatch func(s *service, req *sip.Request, tx sip.ServerTransaction)
	}{
		{
			name:   "REGISTER multiple handlers",
			method: sip.REGISTER,
			register: func(s *service, counters []*int32) {
				for _, c := range counters {
					c := c
					s.OnRegister(func(req *sip.Request, tx sip.ServerTransaction) { atomic.AddInt32(c, 1) })
				}
			},
			dispatch: func(s *service, req *sip.Request, tx sip.ServerTransaction) { s.dispatchRegister(req, tx) },
		},
		{
			name:   "INVITE multiple handlers",
			method: sip.INVITE,
			register: func(s *service, counters []*int32) {
				for _, c := range counters {
					c := c
					s.OnInvite(func(req *sip.Request, tx sip.ServerTransaction) { atomic.AddInt32(c, 1) })
				}
			},
			dispatch: func(s *service, req *sip.Request, tx sip.ServerTransaction) { s.dispatchInvite(req, tx) },
		},
		{
			name:   "BYE multiple handlers",
			method: sip.BYE,
			register: func(s *service, counters []*int32) {
				for _, c := range counters {
					c := c
					s.OnBye(func(req *sip.Request, tx sip.ServerTransaction) { atomic.AddInt32(c, 1) })
				}
			},
			dispatch: func(s *service, req *sip.Request, tx sip.ServerTransaction) { s.dispatchBye(req, tx) },
		},
		{
			name:   "MESSAGE multiple handlers",
			method: sip.MESSAGE,
			register: func(s *service, counters []*int32) {
				for _, c := range counters {
					c := c
					s.OnMessage(func(req *sip.Request, tx sip.ServerTransaction) { atomic.AddInt32(c, 1) })
				}
			},
			dispatch: func(s *service, req *sip.Request, tx sip.ServerTransaction) { s.dispatchMessage(req, tx) },
		},
		{
			name:   "SUBSCRIBE multiple handlers",
			method: sip.SUBSCRIBE,
			register: func(s *service, counters []*int32) {
				for _, c := range counters {
					c := c
					s.OnSubscribe(func(req *sip.Request, tx sip.ServerTransaction) { atomic.AddInt32(c, 1) })
				}
			},
			dispatch: func(s *service, req *sip.Request, tx sip.ServerTransaction) { s.dispatchSubscribe(req, tx) },
		},
		{
			name:   "NOTIFY multiple handlers",
			method: sip.NOTIFY,
			register: func(s *service, counters []*int32) {
				for _, c := range counters {
					c := c
					s.OnNotify(func(req *sip.Request, tx sip.ServerTransaction) { atomic.AddInt32(c, 1) })
				}
			},
			dispatch: func(s *service, req *sip.Request, tx sip.ServerTransaction) { s.dispatchNotify(req, tx) },
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newService()
			const handlerCount = 3
			counters := make([]*int32, handlerCount)
			for i := range counters {
				counters[i] = new(int32)
			}

			tc.register(s, counters)

			tx := newMockServerTx()
			req := makeTestRequest(tc.method)
			tc.dispatch(s, req, tx)

			for i, c := range counters {
				if v := atomic.LoadInt32(c); v != 1 {
					t.Errorf("handler %d called %d times, want 1", i, v)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Concurrent Handler Registration
// ---------------------------------------------------------------------------

func TestConcurrentHandlerRegistration(t *testing.T) {
	s := newService()
	const goroutines = 50

	var wg sync.WaitGroup
	wg.Add(goroutines * 6)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			s.OnRegister(func(req *sip.Request, tx sip.ServerTransaction) {})
		}()
		go func() {
			defer wg.Done()
			s.OnInvite(func(req *sip.Request, tx sip.ServerTransaction) {})
		}()
		go func() {
			defer wg.Done()
			s.OnBye(func(req *sip.Request, tx sip.ServerTransaction) {})
		}()
		go func() {
			defer wg.Done()
			s.OnMessage(func(req *sip.Request, tx sip.ServerTransaction) {})
		}()
		go func() {
			defer wg.Done()
			s.OnSubscribe(func(req *sip.Request, tx sip.ServerTransaction) {})
		}()
		go func() {
			defer wg.Done()
			s.OnNotify(func(req *sip.Request, tx sip.ServerTransaction) {})
		}()
	}

	wg.Wait()

	s.mu.RLock()
	defer s.mu.RUnlock()

	if got := len(s.registerHandlers); got != goroutines {
		t.Errorf("register handlers = %d, want %d", got, goroutines)
	}
	if got := len(s.inviteHandlers); got != goroutines {
		t.Errorf("invite handlers = %d, want %d", got, goroutines)
	}
	if got := len(s.byeHandlers); got != goroutines {
		t.Errorf("bye handlers = %d, want %d", got, goroutines)
	}
	if got := len(s.messageHandlers); got != goroutines {
		t.Errorf("message handlers = %d, want %d", got, goroutines)
	}
	if got := len(s.subscribeHandlers); got != goroutines {
		t.Errorf("subscribe handlers = %d, want %d", got, goroutines)
	}
	if got := len(s.notifyHandlers); got != goroutines {
		t.Errorf("notify handlers = %d, want %d", got, goroutines)
	}
}

// ---------------------------------------------------------------------------
// Service Accessors
// ---------------------------------------------------------------------------

func TestServiceAccessors(t *testing.T) {
	s := newService()
	s.localAddr = "0.0.0.0:5060"
	s.serverID = "34020000002000000001"
	s.domain = "3402000000"

	if got := s.LocalAddr(); got != "0.0.0.0:5060" {
		t.Errorf("LocalAddr() = %q, want %q", got, "0.0.0.0:5060")
	}
	if got := s.ServerID(); got != "34020000002000000001" {
		t.Errorf("ServerID() = %q, want %q", got, "34020000002000000001")
	}
	if got := s.Domain(); got != "3402000000" {
		t.Errorf("Domain() = %q, want %q", got, "3402000000")
	}
}

func TestServiceAccessorsDefault(t *testing.T) {
	s := newService()

	if got := s.LocalAddr(); got != "" {
		t.Errorf("LocalAddr() on new service = %q, want empty string", got)
	}
	if got := s.ServerID(); got != "" {
		t.Errorf("ServerID() on new service = %q, want empty string", got)
	}
	if got := s.Domain(); got != "" {
		t.Errorf("Domain() on new service = %q, want empty string", got)
	}
}

// ---------------------------------------------------------------------------
// Dispatch Does Not Send Auto-Response When Handler Present
// ---------------------------------------------------------------------------

func TestDispatchHandlerReceivesCorrectRequest(t *testing.T) {
	s := newService()
	var received *sip.Request

	s.OnRegister(func(req *sip.Request, tx sip.ServerTransaction) {
		received = req
	})

	tx := newMockServerTx()
	req := makeTestRequest(sip.REGISTER)
	s.dispatchRegister(req, tx)

	if received == nil {
		t.Fatal("handler did not receive the request")
	}
	if received != req {
		t.Error("handler received a different request object than what was dispatched")
	}
}

func TestDispatchHandlerReceivesCorrectTransaction(t *testing.T) {
	s := newService()
	var receivedTx sip.ServerTransaction

	s.OnInvite(func(req *sip.Request, tx sip.ServerTransaction) {
		receivedTx = tx
	})

	tx := newMockServerTx()
	req := makeTestRequest(sip.INVITE)
	s.dispatchInvite(req, tx)

	if receivedTx == nil {
		t.Fatal("handler did not receive the transaction")
	}
	if receivedTx != tx {
		t.Error("handler received a different transaction object than what was dispatched")
	}
}
