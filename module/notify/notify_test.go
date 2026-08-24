package notify

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestWebhookFailureLogsRedactedEndpoint(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("network failure included secret-query-value")
	})}
	ep := config.NotifyEndpointConfig{
		URL: "https://webhook-user:webhook-password@example.test/private/signature-value?token=secret-query-value&signature=query-signature",
	}
	NewHTTPSender(nil).deliverToEndpoint(client, ep, []byte(`{"event":"test"}`), 1)

	output := logs.String()
	if !strings.Contains(output, "https://example.test") {
		t.Fatalf("sanitized endpoint origin missing from log: %s", output)
	}
	for _, secret := range []string{"webhook-user", "webhook-password", "private", "signature-value", "secret-query-value", "query-signature", "token="} {
		if strings.Contains(output, secret) {
			t.Fatalf("webhook log exposed %q: %s", secret, output)
		}
	}
}

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

// TestHTTPSenderHMACSignatureVerification verifies that the X-Signature header
// contains a valid HMAC-SHA256 of the JSON body using the configured secret.
// This is a more thorough test than TestHTTPSenderHMACSignature, verifying the
// raw signature computation independently.
func TestHTTPSenderHMACSignatureVerification(t *testing.T) {
	secret := "webhook-secret-key-123"
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

	payload := &NotifyPayload{
		Event:     "on_subscribe",
		StreamKey: "live/hmac-verify",
		Protocol:  "http-flv",
		Timestamp: 1700000000,
		Extra:     map[string]any{"client_id": "abc123"},
	}
	sender.Send(payload)

	time.Sleep(200 * time.Millisecond)
	sender.Stop()

	mu.Lock()
	sig := receivedSig
	body := receivedBody
	mu.Unlock()

	if sig == "" {
		t.Fatal("expected X-Signature header to be set")
	}
	if len(body) == 0 {
		t.Fatal("expected non-empty request body")
	}

	// Independently compute HMAC-SHA256
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expectedSig := hex.EncodeToString(mac.Sum(nil))

	if sig != expectedSig {
		t.Errorf("HMAC signature mismatch:\n  got:  %s\n  want: %s", sig, expectedSig)
	}

	// Verify the body can be decoded back to the payload
	var decoded NotifyPayload
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("failed to decode body: %v", err)
	}
	if decoded.Event != "on_subscribe" {
		t.Errorf("expected event on_subscribe, got %s", decoded.Event)
	}
	if decoded.StreamKey != "live/hmac-verify" {
		t.Errorf("expected stream_key live/hmac-verify, got %s", decoded.StreamKey)
	}
}

// TestHTTPSenderNoSignatureWithoutSecret verifies that when no secret is
// configured, the X-Signature header is absent.
func TestHTTPSenderNoSignatureWithoutSecret(t *testing.T) {
	var mu sync.Mutex
	var receivedSig string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		receivedSig = r.Header.Get("X-Signature")
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer ts.Close()

	endpoints := []config.NotifyEndpointConfig{
		{URL: ts.URL, Secret: "", Retry: 1, Timeout: 2 * time.Second},
	}
	sender := NewHTTPSender(endpoints)
	sender.Start()

	sender.Send(&NotifyPayload{Event: "on_publish", StreamKey: "live/nosig", Timestamp: 1})

	time.Sleep(200 * time.Millisecond)
	sender.Stop()

	mu.Lock()
	sig := receivedSig
	mu.Unlock()

	if sig != "" {
		t.Errorf("expected no X-Signature header when secret is empty, got %s", sig)
	}
}

// TestHTTPSenderRetryBehavior verifies that the sender retries delivery when
// the endpoint returns a server error, and eventually succeeds.
func TestHTTPSenderRetryBehavior(t *testing.T) {
	var mu sync.Mutex
	var attempts int

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		current := attempts
		mu.Unlock()

		if current < 3 {
			// Fail the first two attempts
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		// Succeed on the third attempt
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	endpoints := []config.NotifyEndpointConfig{
		{URL: ts.URL, Retry: 3, Timeout: 2 * time.Second},
	}
	sender := NewHTTPSender(endpoints)
	sender.Start()

	sender.Send(&NotifyPayload{Event: "on_publish", StreamKey: "live/retry", Timestamp: 1})

	// Wait long enough for retries (backoff: 1s, 2s)
	time.Sleep(5 * time.Second)
	sender.Stop()

	mu.Lock()
	finalAttempts := attempts
	mu.Unlock()

	if finalAttempts != 3 {
		t.Errorf("expected 3 attempts (2 failures + 1 success), got %d", finalAttempts)
	}
}

// TestHTTPSenderRetryExhausted verifies that after all retries are exhausted,
// no further attempts are made.
func TestHTTPSenderRetryExhausted(t *testing.T) {
	var mu sync.Mutex
	var attempts int

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		mu.Unlock()
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer ts.Close()

	endpoints := []config.NotifyEndpointConfig{
		{URL: ts.URL, Retry: 2, Timeout: 2 * time.Second},
	}
	sender := NewHTTPSender(endpoints)
	sender.Start()

	sender.Send(&NotifyPayload{Event: "on_publish", StreamKey: "live/exhaust", Timestamp: 1})

	// Wait for retries (backoff: 1s)
	time.Sleep(3 * time.Second)
	sender.Stop()

	mu.Lock()
	finalAttempts := attempts
	mu.Unlock()

	if finalAttempts != 2 {
		t.Errorf("expected exactly 2 attempts (all exhausted), got %d", finalAttempts)
	}
}

// TestHTTPSenderQueueOverflow verifies that when the queue is full, new events
// are dropped without blocking the caller.
func TestHTTPSenderQueueOverflow(t *testing.T) {
	// Create a sender with a tiny queue but do NOT start the worker,
	// so the queue cannot be drained.
	endpoints := []config.NotifyEndpointConfig{
		{URL: "http://localhost:1/noop", Retry: 1, Timeout: 1 * time.Second},
	}
	sender := &HTTPSender{
		endpoints: endpoints,
		queue:     make(chan *NotifyPayload, 2), // capacity of 2
		done:      make(chan struct{}),
	}
	// Do not call sender.Start() — worker is not running, so queue stays full.

	// Fill the queue
	sender.Send(&NotifyPayload{Event: "on_publish", StreamKey: "live/a", Timestamp: 1})
	sender.Send(&NotifyPayload{Event: "on_publish", StreamKey: "live/b", Timestamp: 2})

	// This should be dropped (queue full) without blocking
	done := make(chan struct{})
	go func() {
		sender.Send(&NotifyPayload{Event: "on_publish", StreamKey: "live/c", Timestamp: 3})
		close(done)
	}()

	select {
	case <-done:
		// Send returned without blocking — correct behavior
	case <-time.After(1 * time.Second):
		t.Fatal("Send() blocked when queue was full; expected non-blocking drop")
	}

	// Queue should have exactly 2 items (capacity)
	if len(sender.queue) != 2 {
		t.Errorf("expected queue length 2, got %d", len(sender.queue))
	}
}

// TestHTTPSenderMultipleEndpoints verifies that a single event is delivered
// to all matching endpoints.
func TestHTTPSenderMultipleEndpoints(t *testing.T) {
	var mu sync.Mutex
	received := make(map[string]int)

	makeServer := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			received[name]++
			mu.Unlock()
			w.WriteHeader(200)
		}))
	}

	ts1 := makeServer("endpoint1")
	defer ts1.Close()
	ts2 := makeServer("endpoint2")
	defer ts2.Close()
	ts3 := makeServer("endpoint3")
	defer ts3.Close()

	endpoints := []config.NotifyEndpointConfig{
		{URL: ts1.URL, Retry: 1, Timeout: 2 * time.Second},
		{URL: ts2.URL, Retry: 1, Timeout: 2 * time.Second},
		{URL: ts3.URL, Retry: 1, Timeout: 2 * time.Second},
	}
	sender := NewHTTPSender(endpoints)
	sender.Start()

	sender.Send(&NotifyPayload{Event: "on_publish", StreamKey: "live/multi", Timestamp: 1})

	time.Sleep(300 * time.Millisecond)
	sender.Stop()

	mu.Lock()
	defer mu.Unlock()

	for _, name := range []string{"endpoint1", "endpoint2", "endpoint3"} {
		if received[name] != 1 {
			t.Errorf("expected %s to receive 1 event, got %d", name, received[name])
		}
	}
}

// TestHTTPSenderMultipleEndpointsWithFilters verifies that events are delivered
// only to endpoints whose event filter matches.
func TestHTTPSenderMultipleEndpointsWithFilters(t *testing.T) {
	var mu sync.Mutex
	received := make(map[string][]string)

	makeServer := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			var p NotifyPayload
			json.Unmarshal(body, &p)
			mu.Lock()
			received[name] = append(received[name], p.Event)
			mu.Unlock()
			w.WriteHeader(200)
		}))
	}

	ts1 := makeServer("pub_only")
	defer ts1.Close()
	ts2 := makeServer("sub_only")
	defer ts2.Close()
	ts3 := makeServer("all_events")
	defer ts3.Close()

	endpoints := []config.NotifyEndpointConfig{
		{URL: ts1.URL, Events: []string{"on_publish"}, Retry: 1, Timeout: 2 * time.Second},
		{URL: ts2.URL, Events: []string{"on_subscribe"}, Retry: 1, Timeout: 2 * time.Second},
		{URL: ts3.URL, Retry: 1, Timeout: 2 * time.Second}, // empty filter = all events
	}
	sender := NewHTTPSender(endpoints)
	sender.Start()

	sender.Send(&NotifyPayload{Event: "on_publish", StreamKey: "live/a", Timestamp: 1})
	sender.Send(&NotifyPayload{Event: "on_subscribe", StreamKey: "live/a", Timestamp: 2})

	time.Sleep(300 * time.Millisecond)
	sender.Stop()

	mu.Lock()
	defer mu.Unlock()

	// pub_only should receive only on_publish
	if len(received["pub_only"]) != 1 || received["pub_only"][0] != "on_publish" {
		t.Errorf("pub_only expected [on_publish], got %v", received["pub_only"])
	}

	// sub_only should receive only on_subscribe
	if len(received["sub_only"]) != 1 || received["sub_only"][0] != "on_subscribe" {
		t.Errorf("sub_only expected [on_subscribe], got %v", received["sub_only"])
	}

	// all_events should receive both
	if len(received["all_events"]) != 2 {
		t.Errorf("all_events expected 2 events, got %d: %v", len(received["all_events"]), received["all_events"])
	}
}

func TestHTTPSenderStopWaitsForQueuedAndInFlightDeliveryAndIsIdempotent(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var mu sync.Mutex
	deliveries := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		mu.Lock()
		deliveries++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	sender := NewHTTPSender([]config.NotifyEndpointConfig{{URL: server.URL, Retry: 1, Timeout: time.Second}})
	sender.Start()
	sender.Send(&NotifyPayload{Event: "on_publish", StreamKey: "live/one"})
	sender.Send(&NotifyPayload{Event: "on_publish", StreamKey: "live/two"})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first delivery did not start")
	}

	stopped := make(chan struct{}, 2)
	go func() { sender.Stop(); stopped <- struct{}{} }()
	go func() { sender.Stop(); stopped <- struct{}{} }()
	select {
	case <-stopped:
		t.Fatal("Stop returned while a delivery was in flight")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	for i := 0; i < 2; i++ {
		select {
		case <-stopped:
		case <-time.After(time.Second):
			t.Fatal("idempotent Stop did not return after queue drain")
		}
	}
	mu.Lock()
	got := deliveries
	mu.Unlock()
	if got != 2 {
		t.Fatalf("deliveries = %d, want queued and in-flight events", got)
	}
}

func TestHTTPSenderStopWithTimeoutIsBounded(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	sender := NewHTTPSender([]config.NotifyEndpointConfig{{URL: server.URL, Retry: 1, Timeout: time.Second}})
	sender.Start()
	sender.Send(&NotifyPayload{Event: "on_publish", StreamKey: "live/blocked"})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("delivery did not start")
	}

	start := time.Now()
	if sender.StopWithTimeout(20 * time.Millisecond) {
		t.Fatal("bounded stop reported completion while delivery remained blocked")
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("bounded stop exceeded timeout: %v", elapsed)
	}
	close(release)
	if !sender.StopWithTimeout(time.Second) {
		t.Fatal("second stop did not observe worker completion")
	}
}

func TestModuleCloseUsesConfiguredDrainTimeout(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	sender := NewHTTPSender([]config.NotifyEndpointConfig{{URL: server.URL, Retry: 1, Timeout: time.Second}})
	sender.Start()
	sender.Send(&NotifyPayload{Event: "on_publish", StreamKey: "live/module-close"})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("delivery did not start")
	}
	module := &Module{sender: sender, drainTimeout: 20 * time.Millisecond}

	start := time.Now()
	if err := module.Close(); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("module close exceeded configured drain timeout: %v", elapsed)
	}
	close(release)
	if !sender.StopWithTimeout(time.Second) {
		t.Fatal("sender did not finish after blocked delivery was released")
	}
}
