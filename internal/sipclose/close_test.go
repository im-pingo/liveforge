package sipclose

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

func TestCloseUserAgentAvoidsUDPReferenceUnderflow(t *testing.T) {
	var logs bytes.Buffer
	sip.SetDefaultLogger(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer sip.SetDefaultLogger(nil)

	ua, err := sipgo.NewUA(sipgo.WithUserAgentHostname("close-test.local"))
	if err != nil {
		t.Fatalf("NewUA: %v", err)
	}
	client, err := sipgo.NewClient(ua, sipgo.WithClientConnectionAddr("127.0.0.1:0"))
	if err != nil {
		_ = ua.Close()
		t.Fatalf("NewClient: %v", err)
	}
	req := sip.NewRequest(sip.OPTIONS, sip.Uri{Scheme: "sip", Host: "127.0.0.1", Port: 9})
	req.AppendHeader(&sip.ViaHeader{Host: "127.0.0.1", Port: 0})
	req.SetTransport("udp")
	if err := sipgo.ClientRequestBuild(client, req); err != nil {
		_ = ua.Close()
		t.Fatalf("ClientRequestBuild: %v", err)
	}
	conn, err := client.TransportLayer().ClientRequestConnection(context.Background(), req)
	if err != nil {
		_ = ua.Close()
		t.Fatalf("ClientRequestConnection: %v", err)
	}

	if err := CloseUserAgent(ua, []sip.Connection{conn}); err != nil {
		t.Fatalf("CloseUserAgent: %v", err)
	}
	if strings.Contains(logs.String(), "UDP ref went negative") {
		t.Fatalf("UserAgent shutdown underflowed UDP reference count: %s", logs.String())
	}
	if strings.Contains(logs.String(), "connection pool not clean cleanup") {
		t.Fatalf("UserAgent shutdown closed a SIP socket before its reader: %s", logs.String())
	}
}
