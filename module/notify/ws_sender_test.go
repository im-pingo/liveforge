package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestParseEventFilter(t *testing.T) {
	if f := parseEventFilter(""); f != nil {
		t.Error("empty string should return nil")
	}
	if f := parseEventFilter("  "); f != nil {
		t.Error("whitespace should return nil")
	}

	f := parseEventFilter("on_publish,on_subscribe")
	if f == nil || !f["on_publish"] || !f["on_subscribe"] {
		t.Errorf("expected on_publish and on_subscribe, got %v", f)
	}
	if f["on_stream_create"] {
		t.Error("should not contain on_stream_create")
	}

	f = parseEventFilter(" on_publish , on_subscribe ")
	if f == nil || !f["on_publish"] || !f["on_subscribe"] {
		t.Errorf("expected trimmed parsing, got %v", f)
	}
}

func TestWSSenderBroadcast(t *testing.T) {
	sender := NewWSSender()
	defer sender.Close()

	ts := httptest.NewServer(http.HandlerFunc(sender.HandleWebSocket))
	defer ts.Close()

	addr := "ws" + ts.URL[4:] // http:// -> ws://

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, addr, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	// Wait for client to be registered
	time.Sleep(50 * time.Millisecond)

	if sender.ClientCount() != 1 {
		t.Fatalf("expected 1 client, got %d", sender.ClientCount())
	}

	// Send a notification
	sender.Send(&NotifyPayload{
		Event:     "on_publish",
		StreamKey: "live/test",
		Timestamp: time.Now().Unix(),
	})

	// Read the message
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	var p NotifyPayload
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.Event != "on_publish" {
		t.Errorf("expected on_publish, got %s", p.Event)
	}
	if p.StreamKey != "live/test" {
		t.Errorf("expected live/test, got %s", p.StreamKey)
	}
}

func TestWSSenderEventFilter(t *testing.T) {
	sender := NewWSSender()
	defer sender.Close()

	ts := httptest.NewServer(http.HandlerFunc(sender.HandleWebSocket))
	defer ts.Close()

	addr := "ws" + ts.URL[4:]

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Connect with event filter
	conn, _, err := websocket.Dial(ctx, addr+"?events=on_publish", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	time.Sleep(50 * time.Millisecond)

	// Send a non-matching event
	sender.Send(&NotifyPayload{
		Event:     "on_subscribe",
		StreamKey: "live/other",
		Timestamp: time.Now().Unix(),
	})

	// Send a matching event
	sender.Send(&NotifyPayload{
		Event:     "on_publish",
		StreamKey: "live/test",
		Timestamp: time.Now().Unix(),
	})

	// Should receive only the matching event
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	var p NotifyPayload
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.Event != "on_publish" {
		t.Errorf("expected on_publish, got %s", p.Event)
	}
}

func TestWSSenderMultipleClients(t *testing.T) {
	sender := NewWSSender()
	defer sender.Close()

	ts := httptest.NewServer(http.HandlerFunc(sender.HandleWebSocket))
	defer ts.Close()

	addr := "ws" + ts.URL[4:]

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn1, _, err := websocket.Dial(ctx, addr, nil)
	if err != nil {
		t.Fatalf("dial1: %v", err)
	}
	defer conn1.Close(websocket.StatusNormalClosure, "")

	conn2, _, err := websocket.Dial(ctx, addr, nil)
	if err != nil {
		t.Fatalf("dial2: %v", err)
	}
	defer conn2.Close(websocket.StatusNormalClosure, "")

	time.Sleep(50 * time.Millisecond)

	if sender.ClientCount() != 2 {
		t.Fatalf("expected 2 clients, got %d", sender.ClientCount())
	}

	sender.Send(&NotifyPayload{
		Event:     "on_publish",
		StreamKey: "live/multi",
		Timestamp: time.Now().Unix(),
	})

	// Both clients should receive
	for i, conn := range []*websocket.Conn{conn1, conn2} {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("read conn%d: %v", i+1, err)
		}
		var p NotifyPayload
		json.Unmarshal(data, &p)
		if p.StreamKey != "live/multi" {
			t.Errorf("conn%d: expected live/multi, got %s", i+1, p.StreamKey)
		}
	}
}

func TestWSSenderClientDisconnect(t *testing.T) {
	sender := NewWSSender()
	defer sender.Close()

	ts := httptest.NewServer(http.HandlerFunc(sender.HandleWebSocket))
	defer ts.Close()

	addr := "ws" + ts.URL[4:]

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, addr, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	time.Sleep(50 * time.Millisecond)
	if sender.ClientCount() != 1 {
		t.Fatalf("expected 1 client, got %d", sender.ClientCount())
	}

	conn.Close(websocket.StatusNormalClosure, "bye")
	time.Sleep(100 * time.Millisecond)

	if sender.ClientCount() != 0 {
		t.Errorf("expected 0 clients after disconnect, got %d", sender.ClientCount())
	}
}

func TestWSSenderClose(t *testing.T) {
	sender := NewWSSender()

	ts := httptest.NewServer(http.HandlerFunc(sender.HandleWebSocket))
	defer ts.Close()

	addr := "ws" + ts.URL[4:]

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, addr, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = conn

	time.Sleep(50 * time.Millisecond)

	sender.Close()

	if sender.ClientCount() != 0 {
		t.Errorf("expected 0 clients after Close, got %d", sender.ClientCount())
	}
}
