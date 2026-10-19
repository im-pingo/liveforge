package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestWSSenderOriginPolicy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		origin func(string) string
		allow  bool
	}{
		{name: "native client without Origin", origin: func(string) string { return "" }, allow: true},
		{name: "same host browser", origin: func(serverURL string) string { return serverURL }, allow: true},
		{name: "different host browser", origin: func(string) string { return "http://untrusted.example" }},
		{name: "different port browser", origin: func(serverURL string) string {
			return serverURL[:strings.LastIndex(serverURL, ":")] + ":1"
		}},
		{name: "opaque browser origin", origin: func(string) string { return "null" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sender := NewWSSender()
			defer sender.Close()
			ts := httptest.NewServer(http.HandlerFunc(sender.HandleWebSocket))
			defer ts.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			header := make(http.Header)
			if origin := tc.origin(ts.URL); origin != "" {
				header.Set("Origin", origin)
			}
			conn, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http"), &websocket.DialOptions{HTTPHeader: header})
			if conn != nil {
				defer conn.CloseNow()
			}
			if !tc.allow {
				if err == nil {
					t.Fatal("untrusted Origin completed a notification WebSocket handshake")
				}
				if resp == nil || resp.StatusCode != http.StatusForbidden {
					t.Fatalf("handshake response = %v, want HTTP 403", resp)
				}
				if got := sender.ClientCount(); got != 0 {
					t.Fatalf("rejected Origin registered %d clients, want 0", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("trusted notification handshake: %v", err)
			}

			type received struct {
				data []byte
				err  error
			}
			result := make(chan received, 1)
			go func() {
				_, data, err := conn.Read(ctx)
				result <- received{data: data, err: err}
			}()
			// Read processes the pong; its receipt proves server registration.
			if err := conn.Ping(ctx); err != nil {
				t.Fatalf("wait for registered client: %v", err)
			}
			sender.Send(&NotifyPayload{Event: "on_publish", StreamKey: "live/origin-policy"})
			var got received
			select {
			case got = <-result:
			case <-ctx.Done():
				t.Fatal("accepted client did not receive notification")
			}
			if got.err != nil {
				t.Fatalf("read notification: %v", got.err)
			}
			var payload NotifyPayload
			if err := json.Unmarshal(got.data, &payload); err != nil {
				t.Fatalf("decode notification: %v", err)
			}
			if payload.Event != "on_publish" || payload.StreamKey != "live/origin-policy" {
				t.Fatalf("notification = %+v, want on_publish for live/origin-policy", payload)
			}
		})
	}
}
