package notify

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
)

func TestBuildPayload(t *testing.T) {
	ctx := &core.EventContext{
		StreamKey:  "live/test",
		Protocol:   "rtmp",
		RemoteAddr: "1.2.3.4:5678",
		Extra:      map[string]any{"bytes_in": int64(1234)},
	}
	p := BuildPayload("on_publish", ctx)
	if p.Event != "on_publish" {
		t.Errorf("expected event on_publish, got %s", p.Event)
	}
	if p.StreamKey != "live/test" {
		t.Errorf("expected stream_key live/test, got %s", p.StreamKey)
	}
	if p.Protocol != "rtmp" {
		t.Errorf("expected protocol rtmp, got %s", p.Protocol)
	}
	if p.Timestamp == 0 {
		t.Error("expected non-zero timestamp")
	}
}

func TestMatchEvent(t *testing.T) {
	// Empty filter matches everything
	if !matchEvent(nil, "on_publish") {
		t.Error("nil filter should match all")
	}
	if !matchEvent([]string{}, "on_publish") {
		t.Error("empty filter should match all")
	}
	// Specific filter
	if !matchEvent([]string{"on_publish", "on_publish_stop"}, "on_publish") {
		t.Error("should match on_publish")
	}
	if matchEvent([]string{"on_publish"}, "on_subscribe") {
		t.Error("should not match on_subscribe")
	}
}

func TestComputeHMAC(t *testing.T) {
	data := []byte(`{"event":"on_publish"}`)
	secret := "test-secret"
	sig := computeHMAC(data, secret)

	// Verify manually
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(data)
	expected := hex.EncodeToString(mac.Sum(nil))
	if sig != expected {
		t.Errorf("HMAC mismatch: got %s, want %s", sig, expected)
	}
}

func TestHTTPSenderDelivery(t *testing.T) {
	var mu sync.Mutex
	var received []NotifyPayload

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var p NotifyPayload
		json.Unmarshal(body, &p)
		mu.Lock()
		received = append(received, p)
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer ts.Close()

	endpoints := []config.NotifyEndpointConfig{
		{URL: ts.URL, Retry: 1, Timeout: 2 * time.Second},
	}
	sender := NewHTTPSender(endpoints)
	sender.Start()

	sender.Send(&NotifyPayload{
		Event:     "on_publish",
		StreamKey: "live/test",
		Protocol:  "rtmp",
		Timestamp: time.Now().Unix(),
	})

	// Wait for delivery
	time.Sleep(200 * time.Millisecond)
	sender.Stop()

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 {
		t.Fatalf("expected 1 delivery, got %d", len(received))
	}
	if received[0].Event != "on_publish" {
		t.Errorf("expected event on_publish, got %s", received[0].Event)
	}
	if received[0].StreamKey != "live/test" {
		t.Errorf("expected stream_key live/test, got %s", received[0].StreamKey)
	}
}

func TestHTTPSenderHMACSignature(t *testing.T) {
	secret := "my-webhook-secret"
	var mu sync.Mutex
	var receivedSig string
	var receivedBody []byte

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		receivedSig = r.Header.Get("X-Signature")
		receivedBody = body
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer ts.Close()

	endpoints := []config.NotifyEndpointConfig{
		{URL: ts.URL, Secret: secret, Retry: 1, Timeout: 2 * time.Second},
	}
	sender := NewHTTPSender(endpoints)
	sender.Start()

	sender.Send(&NotifyPayload{
		Event:     "on_publish",
		StreamKey: "live/sig",
		Timestamp: 1234567890,
	})

	time.Sleep(200 * time.Millisecond)
	sender.Stop()

	mu.Lock()
	sig := receivedSig
	body := receivedBody
	mu.Unlock()

	if sig == "" {
		t.Fatal("expected X-Signature header")
	}

	expected := computeHMAC(body, secret)
	if sig != expected {
		t.Errorf("signature mismatch: got %s, want %s", sig, expected)
	}
}

func TestHTTPSenderEventFilter(t *testing.T) {
	var count int
	var mu sync.Mutex

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		count++
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer ts.Close()

	endpoints := []config.NotifyEndpointConfig{
		{URL: ts.URL, Events: []string{"on_publish"}, Retry: 1, Timeout: 2 * time.Second},
	}
	sender := NewHTTPSender(endpoints)
	sender.Start()

	// This should be delivered (matches filter)
	sender.Send(&NotifyPayload{Event: "on_publish", StreamKey: "live/a", Timestamp: 1})
	// This should be filtered out
	sender.Send(&NotifyPayload{Event: "on_subscribe", StreamKey: "live/b", Timestamp: 2})

	time.Sleep(300 * time.Millisecond)
	sender.Stop()

	mu.Lock()
	defer mu.Unlock()
	if count != 1 {
		t.Errorf("expected 1 delivery (filtered), got %d", count)
	}
}

func TestModuleHooks(t *testing.T) {
	m := NewModule()
	cfg := &config.Config{
		Notify: config.NotifyConfig{
			HTTP: config.NotifyHTTPConfig{Enabled: true},
		},
	}
	s := core.NewServer(cfg)
	if err := m.Init(s); err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	hooks := m.Hooks()
	if len(hooks) != len(eventMapping) {
		t.Errorf("expected %d hooks, got %d", len(eventMapping), len(hooks))
	}
	for _, h := range hooks {
		if h.Mode != core.HookAsync {
			t.Errorf("expected async hook, got %v", h.Mode)
		}
		if h.Priority != 90 {
			t.Errorf("expected priority 90, got %d", h.Priority)
		}
	}
}
