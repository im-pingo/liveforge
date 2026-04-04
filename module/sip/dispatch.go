package sip

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/im-pingo/liveforge/config"
)

// sipClient wraps sipgo Client for sending requests.
type sipClient struct {
	client *sipgo.Client
}

func (c *sipClient) writeRequest(ctx context.Context, req *sip.Request) error {
	_, err := c.client.TransactionRequest(ctx, req)
	return err
}

// service is the concrete implementation of SIPService.
type service struct {
	mu sync.RWMutex

	registerHandlers  []RegisterHandler
	inviteHandlers    []InviteHandler
	byeHandlers       []ByeHandler
	messageHandlers   []MessageHandler
	subscribeHandlers []SubscribeHandler
	notifyHandlers    []NotifyHandler

	ua        *sipgo.UserAgent
	server    *sipgo.Server
	client    *sipClient
	localAddr string
	serverID  string
	domain    string

	cancelFunc context.CancelFunc
}

func newService() *service {
	return &service{}
}

func (s *service) init(cfg config.SIPConfig) error {
	ua, err := sipgo.NewUA(
		sipgo.WithUserAgent("LiveForge"),
		sipgo.WithUserAgentHostname(cfg.Domain),
	)
	if err != nil {
		return fmt.Errorf("sip: create UA: %w", err)
	}
	s.ua = ua

	srv, err := sipgo.NewServer(ua)
	if err != nil {
		return fmt.Errorf("sip: create server: %w", err)
	}
	s.server = srv

	client, err := sipgo.NewClient(ua)
	if err != nil {
		return fmt.Errorf("sip: create client: %w", err)
	}
	s.client = &sipClient{client: client}

	s.serverID = cfg.ServerID
	s.domain = cfg.Domain
	s.localAddr = cfg.Listen

	// Register SIP method handlers
	srv.OnRegister(func(req *sip.Request, tx sip.ServerTransaction) {
		s.dispatchRegister(req, tx)
	})
	srv.OnInvite(func(req *sip.Request, tx sip.ServerTransaction) {
		s.dispatchInvite(req, tx)
	})
	srv.OnBye(func(req *sip.Request, tx sip.ServerTransaction) {
		s.dispatchBye(req, tx)
	})
	srv.OnMessage(func(req *sip.Request, tx sip.ServerTransaction) {
		s.dispatchMessage(req, tx)
	})
	srv.OnSubscribe(func(req *sip.Request, tx sip.ServerTransaction) {
		s.dispatchSubscribe(req, tx)
	})
	srv.OnNotify(func(req *sip.Request, tx sip.ServerTransaction) {
		s.dispatchNotify(req, tx)
	})

	// Start listeners
	ctx, cancel := context.WithCancel(context.Background())
	s.cancelFunc = cancel

	for _, transport := range cfg.Transport {
		transport := transport
		go func() {
			if err := srv.ListenAndServe(ctx, transport, cfg.Listen); err != nil {
				slog.Error("sip listener stopped", "module", "sip", "transport", transport, "error", err)
			}
		}()
		slog.Info("listening", "module", "sip", "transport", transport, "addr", cfg.Listen)
	}

	return nil
}

func (s *service) close() {
	if s.cancelFunc != nil {
		s.cancelFunc()
	}
}

// --- SIPService interface ---

func (s *service) OnRegister(handler RegisterHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.registerHandlers = append(s.registerHandlers, handler)
}

func (s *service) OnInvite(handler InviteHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inviteHandlers = append(s.inviteHandlers, handler)
}

func (s *service) OnBye(handler ByeHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byeHandlers = append(s.byeHandlers, handler)
}

func (s *service) OnMessage(handler MessageHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messageHandlers = append(s.messageHandlers, handler)
}

func (s *service) OnSubscribe(handler SubscribeHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subscribeHandlers = append(s.subscribeHandlers, handler)
}

func (s *service) OnNotify(handler NotifyHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notifyHandlers = append(s.notifyHandlers, handler)
}

func (s *service) SendRequest(ctx context.Context, req *sip.Request) (*sip.Response, error) {
	if s.client == nil {
		return nil, fmt.Errorf("sip: client not initialized")
	}
	tx, err := s.client.client.TransactionRequest(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("sip: send request: %w", err)
	}
	defer tx.Terminate()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case resp := <-tx.Responses():
		return resp, nil
	}
}

func (s *service) SendInvite(ctx context.Context, req *sip.Request) (*InviteTransaction, error) {
	if s.client == nil {
		return nil, fmt.Errorf("sip: client not initialized")
	}
	tx, err := s.client.client.TransactionRequest(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("sip: send INVITE: %w", err)
	}

	invTx := &InviteTransaction{
		clientTx: tx,
		client:   s.client,
		request:  req,
		done:     make(chan struct{}),
	}

	// Wait for final response in background
	go func() {
		defer invTx.Close()
		for {
			select {
			case <-ctx.Done():
				return
			case resp, ok := <-tx.Responses():
				if !ok {
					return
				}
				if resp.StatusCode >= 200 {
					invTx.mu.Lock()
					invTx.response = resp
					invTx.mu.Unlock()
					return
				}
			}
		}
	}()

	return invTx, nil
}

func (s *service) LocalAddr() string { return s.localAddr }
func (s *service) ServerID() string  { return s.serverID }
func (s *service) Domain() string    { return s.domain }

// --- dispatch methods ---

func (s *service) dispatchRegister(req *sip.Request, tx sip.ServerTransaction) {
	s.mu.RLock()
	handlers := make([]RegisterHandler, len(s.registerHandlers))
	copy(handlers, s.registerHandlers)
	s.mu.RUnlock()

	if len(handlers) == 0 {
		resp := sip.NewResponseFromRequest(req, 405, "Method Not Allowed", nil)
		tx.Respond(resp)
		return
	}
	for _, h := range handlers {
		h(req, tx)
	}
}

func (s *service) dispatchInvite(req *sip.Request, tx sip.ServerTransaction) {
	s.mu.RLock()
	handlers := make([]InviteHandler, len(s.inviteHandlers))
	copy(handlers, s.inviteHandlers)
	s.mu.RUnlock()

	if len(handlers) == 0 {
		resp := sip.NewResponseFromRequest(req, 405, "Method Not Allowed", nil)
		tx.Respond(resp)
		return
	}
	for _, h := range handlers {
		h(req, tx)
	}
}

func (s *service) dispatchBye(req *sip.Request, tx sip.ServerTransaction) {
	s.mu.RLock()
	handlers := make([]ByeHandler, len(s.byeHandlers))
	copy(handlers, s.byeHandlers)
	s.mu.RUnlock()

	if len(handlers) == 0 {
		resp := sip.NewResponseFromRequest(req, 200, "OK", nil)
		tx.Respond(resp)
		return
	}
	for _, h := range handlers {
		h(req, tx)
	}
}

func (s *service) dispatchMessage(req *sip.Request, tx sip.ServerTransaction) {
	s.mu.RLock()
	handlers := make([]MessageHandler, len(s.messageHandlers))
	copy(handlers, s.messageHandlers)
	s.mu.RUnlock()

	if len(handlers) == 0 {
		resp := sip.NewResponseFromRequest(req, 200, "OK", nil)
		tx.Respond(resp)
		return
	}
	for _, h := range handlers {
		h(req, tx)
	}
}

func (s *service) dispatchSubscribe(req *sip.Request, tx sip.ServerTransaction) {
	s.mu.RLock()
	handlers := make([]SubscribeHandler, len(s.subscribeHandlers))
	copy(handlers, s.subscribeHandlers)
	s.mu.RUnlock()

	if len(handlers) == 0 {
		resp := sip.NewResponseFromRequest(req, 405, "Method Not Allowed", nil)
		tx.Respond(resp)
		return
	}
	for _, h := range handlers {
		h(req, tx)
	}
}

func (s *service) dispatchNotify(req *sip.Request, tx sip.ServerTransaction) {
	s.mu.RLock()
	handlers := make([]NotifyHandler, len(s.notifyHandlers))
	copy(handlers, s.notifyHandlers)
	s.mu.RUnlock()

	if len(handlers) == 0 {
		resp := sip.NewResponseFromRequest(req, 200, "OK", nil)
		tx.Respond(resp)
		return
	}
	for _, h := range handlers {
		h(req, tx)
	}
}
