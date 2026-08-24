package cluster

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
)

func authenticatedPeerServer(t *testing.T, expected *atomic.Value, requests *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		want := "Bearer " + expected.Load().(string)
		if got := r.Header.Get("Authorization"); got != want {
			http.Error(w, fmt.Sprintf("authorization=%q", got), http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("peer-answer"))
	}))
}

func TestRTPTransportAuthUsesBearerTokenAndObservesRotation(t *testing.T) {
	cfg := config.Defaults()
	cfg.API.Auth.BearerToken = "legacy-one"
	cfg.API.Auth.Tokens = []config.APIAuthToken{{Name: "admin", Token: "named-admin", Role: "admin"}}
	server := core.NewServer(cfg)
	transport := NewRTPTransport(defaultClusterRTPConfig(), server)

	var expected atomic.Value
	expected.Store("legacy-one")
	var requests atomic.Int32
	peer := authenticatedPeerServer(t, &expected, &requests)
	defer peer.Close()

	if _, err := transport.postSDP(context.Background(), peer.URL, []byte("offer-one")); err != nil {
		t.Fatalf("first signaling request: %v", err)
	}
	rotated := *cfg
	rotated.API.Auth.BearerToken = "legacy-two"
	server.UpdateConfig(&rotated)
	expected.Store("legacy-two")
	if _, err := transport.postSDP(context.Background(), peer.URL, []byte("offer-two")); err != nil {
		t.Fatalf("rotated signaling request: %v", err)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("peer requests=%d want=2", got)
	}
}

func TestGB28181TransportCredentialUsesFirstAdminTokenAndObservesRotation(t *testing.T) {
	cfg := config.Defaults()
	cfg.API.Auth.Tokens = []config.APIAuthToken{
		{Name: "viewer", Token: "viewer-token", Role: "viewer"},
		{Name: "admin-one", Token: "named-one", Role: "admin"},
		{Name: "admin-two", Token: "named-ignored", Role: "admin"},
	}
	server := core.NewServer(cfg)
	transport := NewGBTransport(config.ClusterGBConfig{}, server)

	var expected atomic.Value
	expected.Store("named-one")
	var requests atomic.Int32
	peer := authenticatedPeerServer(t, &expected, &requests)
	defer peer.Close()

	if _, err := transport.postSignal(context.Background(), peer.URL); err != nil {
		t.Fatalf("first signaling request: %v", err)
	}
	rotated := *cfg
	rotated.API.Auth.Tokens = []config.APIAuthToken{
		{Name: "viewer", Token: "viewer-token", Role: "viewer"},
		{Name: "admin-one", Token: "named-two", Role: "admin"},
	}
	server.UpdateConfig(&rotated)
	expected.Store("named-two")
	if _, err := transport.postSignal(context.Background(), peer.URL); err != nil {
		t.Fatalf("rotated signaling request: %v", err)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("peer requests=%d want=2", got)
	}
}

func TestRTPAndGB28181CredentialFailurePreventsNetworkDispatch(t *testing.T) {
	cfg := config.Defaults()
	cfg.API.Auth.Tokens = []config.APIAuthToken{
		{Name: "viewer", Token: "viewer-token", Role: "viewer"},
		{Name: "operator", Token: "operator-token", Role: "operator"},
	}
	server := core.NewServer(cfg)
	var requests atomic.Int32
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer peer.Close()

	tests := []struct {
		name string
		call func() error
	}{
		{name: "rtp", call: func() error {
			_, err := NewRTPTransport(defaultClusterRTPConfig(), server).postSDP(context.Background(), peer.URL, []byte("offer"))
			return err
		}},
		{name: "gb28181", call: func() error {
			_, err := NewGBTransport(config.ClusterGBConfig{}, server).postSignal(context.Background(), peer.URL)
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.call()
			if err == nil || !strings.Contains(err.Error(), "admin") || !strings.Contains(err.Error(), "credential") {
				t.Fatalf("error=%v want clear missing admin credential error", err)
			}
		})
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("network requests=%d want=0", got)
	}
}
