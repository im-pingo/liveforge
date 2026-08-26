package core

import (
	"context"
	"errors"
	"testing"

	"github.com/im-pingo/liveforge/config"
)

type testAuthorizer struct {
	request AuthorizationRequest
	err     error
}

func (a *testAuthorizer) Authorize(_ context.Context, request AuthorizationRequest) error {
	a.request = request
	return a.err
}

func TestServerAuthorizeUsesExplicitAuthorizer(t *testing.T) {
	s := NewServer(config.Defaults())
	a := &testAuthorizer{err: errors.New("denied")}
	s.SetAuthorizer(a)

	err := s.Authorize(context.Background(), AuthorizationRequest{
		Action:    AuthorizationSubscribe,
		Stage:     AuthorizationPreSession,
		StreamKey: "live/test",
		Protocol:  "http-flv",
		Params:    map[string]string{"token": "secret"},
	})
	if !errors.Is(err, a.err) {
		t.Fatalf("Authorize error = %v, want %v", err, a.err)
	}
	if a.request.Stage != AuthorizationPreSession || a.request.Params["token"] != "secret" {
		t.Fatalf("authorization request = %#v", a.request)
	}
}

func TestServerAuthorizeAdaptsEventHooks(t *testing.T) {
	s := NewServer(config.Defaults())
	want := errors.New("hook denied")
	s.GetEventBus().Register(HookRegistration{
		Event: EventSubscribe,
		Mode:  HookSync,
		Handler: func(ctx *EventContext) error {
			if ctx.StreamKey != "live/test" || ctx.Protocol != "hls" {
				t.Fatalf("event context = %#v", ctx)
			}
			return want
		},
	})
	if err := s.Authorize(context.Background(), AuthorizationRequest{
		Action:    AuthorizationSubscribe,
		StreamKey: "live/test",
		Protocol:  "hls",
	}); !errors.Is(err, want) {
		t.Fatalf("Authorize error = %v, want %v", err, want)
	}
}
