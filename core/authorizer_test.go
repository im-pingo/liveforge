package core

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/im-pingo/liveforge/config"
)

func testServerConfig() *config.Config {
	return &config.Config{Stream: config.StreamConfig{RingBufferSize: 16}}
}

type authorizerFunc func(context.Context, AuthorizationRequest) error

func (f authorizerFunc) Authorize(ctx context.Context, req AuthorizationRequest) error {
	return f(ctx, req)
}

func TestServerAuthorizeAllowsWhenNoAuthorizerInstalled(t *testing.T) {
	s := NewServer(testServerConfig())
	if err := s.Authorize(context.Background(), AuthorizationRequest{
		Action:    AuthorizationPublish,
		Stage:     AuthorizationPreSession,
		StreamKey: "live/test",
	}); err != nil {
		t.Fatalf("default authorizer rejected request: %v", err)
	}
}

func TestServerAuthorizeDelegatesNormalizedRequest(t *testing.T) {
	s := NewServer(testServerConfig())
	wantErr := errors.New("denied")
	want := AuthorizationRequest{
		Action:     AuthorizationSubscribe,
		Stage:      AuthorizationPostConnect,
		StreamKey:  "live/test",
		Protocol:   "whep",
		RemoteAddr: "127.0.0.1:1234",
		Params:     map[string]string{"token": "secret"},
	}
	var got AuthorizationRequest
	s.SetAuthorizer(authorizerFunc(func(_ context.Context, req AuthorizationRequest) error {
		got = req
		return wantErr
	}))

	err := s.Authorize(context.Background(), want)
	if !errors.Is(err, wantErr) {
		t.Fatalf("Authorize error = %v, want %v", err, wantErr)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("authorizer request = %#v, want %#v", got, want)
	}
}
