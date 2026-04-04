package sip

import (
	"context"

	"github.com/emiago/sipgo/sip"
)

// RegisterHandler handles SIP REGISTER requests.
type RegisterHandler func(req *sip.Request, tx sip.ServerTransaction)

// InviteHandler handles SIP INVITE requests.
type InviteHandler func(req *sip.Request, tx sip.ServerTransaction)

// ByeHandler handles SIP BYE requests.
type ByeHandler func(req *sip.Request, tx sip.ServerTransaction)

// MessageHandler handles SIP MESSAGE requests.
type MessageHandler func(req *sip.Request, tx sip.ServerTransaction)

// SubscribeHandler handles SIP SUBSCRIBE requests.
type SubscribeHandler func(req *sip.Request, tx sip.ServerTransaction)

// NotifyHandler handles SIP NOTIFY requests.
type NotifyHandler func(req *sip.Request, tx sip.ServerTransaction)

// SIPService is the interface exposed by the SIP module for dependent modules.
type SIPService interface {
	OnRegister(handler RegisterHandler)
	OnInvite(handler InviteHandler)
	OnBye(handler ByeHandler)
	OnMessage(handler MessageHandler)
	OnSubscribe(handler SubscribeHandler)
	OnNotify(handler NotifyHandler)

	SendRequest(ctx context.Context, req *sip.Request) (*sip.Response, error)
	SendInvite(ctx context.Context, req *sip.Request) (*InviteTransaction, error)

	LocalAddr() string
	ServerID() string
	Domain() string
}
