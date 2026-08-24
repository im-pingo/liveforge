package cluster

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
)

const testMaxPeerSignalingResponseBytes = 64 << 10

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

	answer, err := transport.postSDP(context.Background(), peer.URL, []byte("offer-one"))
	if err != nil {
		t.Fatalf("first signaling request: %v", err)
	}
	if got := string(answer); got != "peer-answer" {
		t.Fatalf("first signaling response=%q want=peer-answer", got)
	}
	rotated := *cfg
	rotated.API.Auth.BearerToken = "legacy-two"
	server.UpdateConfig(&rotated)
	expected.Store("legacy-two")
	answer, err = transport.postSDP(context.Background(), peer.URL, []byte("offer-two"))
	if err != nil {
		t.Fatalf("rotated signaling request: %v", err)
	}
	if got := string(answer); got != "peer-answer" {
		t.Fatalf("rotated signaling response=%q want=peer-answer", got)
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

	answer, err := transport.postSignal(context.Background(), peer.URL)
	if err != nil {
		t.Fatalf("first signaling request: %v", err)
	}
	if got := string(answer); got != "peer-answer" {
		t.Fatalf("first signaling response=%q want=peer-answer", got)
	}
	rotated := *cfg
	rotated.API.Auth.Tokens = []config.APIAuthToken{
		{Name: "viewer", Token: "viewer-token", Role: "viewer"},
		{Name: "admin-one", Token: "named-two", Role: "admin"},
	}
	server.UpdateConfig(&rotated)
	expected.Store("named-two")
	answer, err = transport.postSignal(context.Background(), peer.URL)
	if err != nil {
		t.Fatalf("rotated signaling request: %v", err)
	}
	if got := string(answer); got != "peer-answer" {
		t.Fatalf("rotated signaling response=%q want=peer-answer", got)
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

func TestRTPTransportAuthRejectsSecretEchoWithoutDisclosure(t *testing.T) {
	const token = "rtp-admin-secret"
	const peerMarker = "peer-echoed-authorization"
	cfg := config.Defaults()
	cfg.API.Auth.BearerToken = token
	server := core.NewServer(cfg)
	peer := rejectingPeerServer(t, peerMarker)
	defer peer.Close()

	_, err := NewRTPTransport(defaultClusterRTPConfig(), server).postSDP(context.Background(), peer.URL, []byte("offer"))
	assertPeerSignalingErrorIsSafe(t, err, token, peerMarker, http.StatusForbidden)
}

func TestGB28181TransportAuthRejectsSecretEchoWithoutDisclosure(t *testing.T) {
	const token = "gb-admin-secret"
	const peerMarker = "peer-echoed-authorization"
	cfg := config.Defaults()
	cfg.API.Auth.BearerToken = token
	server := core.NewServer(cfg)
	peer := rejectingPeerServer(t, peerMarker)
	defer peer.Close()

	_, err := NewGBTransport(config.ClusterGBConfig{}, server).postSignal(context.Background(), peer.URL)
	assertPeerSignalingErrorIsSafe(t, err, token, peerMarker, http.StatusForbidden)
}

func TestRTPTransportAuthBoundsPeerResponse(t *testing.T) {
	server := core.NewServer(config.Defaults())
	peer := oversizedPeerServer(t)
	defer peer.Close()

	_, err := NewRTPTransport(defaultClusterRTPConfig(), server).postSDP(context.Background(), peer.URL, []byte("offer"))
	if err == nil || !strings.Contains(err.Error(), "response too large") {
		t.Fatalf("oversized response error=%v", err)
	}
}

func TestGB28181TransportAuthBoundsPeerResponse(t *testing.T) {
	server := core.NewServer(config.Defaults())
	peer := oversizedPeerServer(t)
	defer peer.Close()

	_, err := NewGBTransport(config.ClusterGBConfig{}, server).postSignal(context.Background(), peer.URL)
	if err == nil || !strings.Contains(err.Error(), "response too large") {
		t.Fatalf("oversized response error=%v", err)
	}
}

func rejectingPeerServer(t *testing.T, marker string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprintf(w, "%s: %s", marker, r.Header.Get("Authorization"))
	}))
}

func oversizedPeerServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(bytes.Repeat([]byte{'x'}, testMaxPeerSignalingResponseBytes+1))
	}))
}

func assertPeerSignalingErrorIsSafe(t *testing.T, err error, token, marker string, status int) {
	t.Helper()
	if err == nil {
		t.Fatal("expected peer rejection")
	}
	wantError := fmt.Sprintf("peer signaling rejected: HTTP %d %s", status, http.StatusText(status))
	if err.Error() != wantError {
		t.Fatalf("error=%q want=%q", err, wantError)
	}
	for _, secret := range []string{token, marker} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error disclosed peer-controlled secret %q: %v", secret, err)
		}
	}

	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	slog.Warn("peer signaling failed", "error", err)
	slog.SetDefault(previousLogger)
	for _, secret := range []string{token, marker} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("log disclosed peer-controlled secret %q: %s", secret, logs.String())
		}
	}
}

type endlessCountingReader struct{ bytesRead int64 }

func (r *endlessCountingReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	r.bytesRead += int64(len(p))
	return len(p), nil
}

func TestPeerSignalingResponseReadsAreBounded(t *testing.T) {
	readBody := &endlessCountingReader{}
	if _, err := readPeerSignalingResponse(readBody); err == nil || !strings.Contains(err.Error(), "response too large") {
		t.Fatalf("bounded read error=%v", err)
	}
	if got, want := readBody.bytesRead, maxPeerSignalingResponseBytes+1; got != want {
		t.Fatalf("success response bytes read=%d want=%d", got, want)
	}

	discardBody := &endlessCountingReader{}
	discardPeerSignalingResponse(discardBody)
	if got, want := discardBody.bytesRead, maxPeerSignalingResponseBytes+1; got != want {
		t.Fatalf("rejected response bytes read=%d want=%d", got, want)
	}
}
