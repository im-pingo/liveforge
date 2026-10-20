package notify

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestWSSenderCloseRejectsLaterHandshakes(t *testing.T) {
	sender := NewWSSender()
	defer sender.Close()
	ts := httptest.NewServer(http.HandlerFunc(sender.HandleWebSocket))
	defer ts.Close()

	sender.Close()
	sender.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, "ws"+ts.URL[4:], nil)
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close()
	}
	if conn != nil {
		defer func() { _ = conn.CloseNow() }()
	}
	if err == nil {
		t.Fatal("WebSocket handshake succeeded after sender Close")
	}
	if resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("handshake response = %v, want HTTP 503", resp)
	}
	if got := sender.ClientCount(); got != 0 {
		t.Fatalf("clients after rejected handshake = %d, want 0", got)
	}
}

func TestWSSenderCloseDuringUpgradeClosesUnregisteredConnection(t *testing.T) {
	sender := NewWSSender()
	defer sender.Close()
	handlerDone := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(handlerDone)
		// Close after the real HTTP connection is hijacked but before Accept
		// returns. This deterministically places shutdown inside the upgrade.
		sender.HandleWebSocket(&closeDuringUpgradeWriter{ResponseWriter: w, closeSender: sender.Close}, r)
	}))
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, "ws"+ts.URL[4:], nil)
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("dial in-progress upgrade: %v", err)
	}
	defer func() { _ = conn.CloseNow() }()

	_, _, err = conn.Read(ctx)
	if got := websocket.CloseStatus(err); got != websocket.StatusGoingAway {
		t.Fatalf("upgrade racing Close ended with %v (status %d), want GoingAway", err, got)
	}
	select {
	case <-handlerDone:
	case <-ctx.Done():
		t.Fatal("handler remained active after shutdown rejected the upgraded connection")
	}
	if got := sender.ClientCount(); got != 0 {
		t.Fatalf("clients after upgrade raced Close = %d, want 0", got)
	}
}

type closeDuringUpgradeWriter struct {
	http.ResponseWriter
	closeSender func()
}

func (w *closeDuringUpgradeWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := w.ResponseWriter.(http.Hijacker).Hijack()
	if err == nil {
		w.closeSender()
	}
	return conn, rw, err
}

func TestWSSenderCloseIsIdempotentWithConnectedClient(t *testing.T) {
	sender := NewWSSender()
	defer sender.Close()
	handlerDone := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(handlerDone)
		sender.HandleWebSocket(w, r)
	}))
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, "ws"+ts.URL[4:], nil)
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.CloseNow() }()
	closed := conn.CloseRead(ctx)
	// A pong proves the server entered its read loop after registration.
	if err := conn.Ping(ctx); err != nil {
		t.Fatalf("wait for registered client: %v", err)
	}
	if got := sender.ClientCount(); got != 1 {
		t.Fatalf("connected clients = %d, want 1", got)
	}

	sender.Close()
	sender.Close()
	select {
	case <-closed.Done():
	case <-ctx.Done():
		t.Fatal("Close did not disconnect the notification client")
	}
	select {
	case <-handlerDone:
	case <-ctx.Done():
		t.Fatal("Close did not stop the notification handler")
	}
	if got := sender.ClientCount(); got != 0 {
		t.Fatalf("clients after repeated Close = %d, want 0", got)
	}
}
