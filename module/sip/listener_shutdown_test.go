package sip

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	sipmessage "github.com/emiago/sipgo/sip"
	"github.com/im-pingo/liveforge/config"
)

const listenerTestBound = 2 * time.Second

type synchronizedLogBuffer struct {
	mu      sync.Mutex
	buffer  bytes.Buffer
	changed chan struct{}
}

func newSynchronizedLogBuffer() *synchronizedLogBuffer {
	return &synchronizedLogBuffer{changed: make(chan struct{}, 16)}
}

func (b *synchronizedLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	n, err := b.buffer.Write(p)
	b.mu.Unlock()
	select {
	case b.changed <- struct{}{}:
	default:
	}
	return n, err
}

func (b *synchronizedLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

func (b *synchronizedLogBuffer) drainChanges() {
	for {
		select {
		case <-b.changed:
		default:
			return
		}
	}
}

func TestSIPListenerShutdownDoesNotLogError(t *testing.T) {
	addr := reserveTCPAddress(t)
	logs := captureDefaultLogs(t)
	svc := newService()
	t.Cleanup(func() {
		svc.close()
		if svc.ua != nil {
			_ = svc.ua.Close()
		}
	})
	if err := svc.init(testSIPConfig(addr)); err != nil {
		t.Fatalf("init SIP service: %v", err)
	}
	waitForTCPListener(t, addr)
	logs.drainChanges()

	closed := make(chan struct{})
	go func() {
		svc.close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(listenerTestBound):
		t.Fatal("SIP service close exceeded shutdown bound")
	}
	waitForTCPListenerRelease(t, addr)

	if output := logs.String(); strings.Contains(output, "level=ERROR") {
		t.Fatalf("normal listener shutdown logged ERROR:\n%s", output)
	}
}

func TestSIPListenerStopFailureWhileContextActiveLogsError(t *testing.T) {
	occupied, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve occupied listener: %v", err)
	}
	t.Cleanup(func() { _ = occupied.Close() })

	logs := captureDefaultLogs(t)
	svc := newService()
	t.Cleanup(func() {
		svc.close()
		if svc.ua != nil {
			_ = svc.ua.Close()
		}
	})
	if err := svc.init(testSIPConfig(occupied.Addr().String())); err != nil {
		t.Fatalf("init SIP service: %v", err)
	}

	waitForLog(t, logs, func(output string) bool {
		return strings.Contains(output, "level=ERROR") &&
			strings.Contains(output, `msg="sip listener stopped"`) &&
			strings.Contains(output, "transport=tcp")
	})
}

func TestSIPListenerStopUnexpectedFailureAfterCancellationLogsError(t *testing.T) {
	logs := captureDefaultLogs(t)
	ua, err := sipgo.NewUA(sipgo.WithUserAgent("LiveForge listener classification test"))
	if err != nil {
		t.Fatalf("create SIP user agent: %v", err)
	}
	t.Cleanup(func() { _ = ua.Close() })
	server, err := sipgo.NewServer(ua)
	if err != nil {
		t.Fatalf("create SIP server: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	newService().serveListener(ctx, server, "unsupported", "127.0.0.1:0")

	output := logs.String()
	if !strings.Contains(output, "level=ERROR") ||
		!strings.Contains(output, `msg="sip listener stopped"`) ||
		!strings.Contains(output, "transport=unsupported") ||
		!strings.Contains(output, sipmessage.ErrTransportNotSuported.Error()) {
		t.Fatalf("unexpected listener failure after cancellation was not logged with transport metadata:\n%s", output)
	}
}

func captureDefaultLogs(t *testing.T) *synchronizedLogBuffer {
	t.Helper()
	logs := newSynchronizedLogBuffer()
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return logs
}

func reserveTCPAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve listener address: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release listener reservation: %v", err)
	}
	return addr
}

func testSIPConfig(addr string) config.SIPConfig {
	return config.SIPConfig{
		Listen:    addr,
		Transport: []string{"tcp"},
		ServerID:  "34020000002000000001",
		Domain:    "3402000000",
	}
}

func waitForTCPListener(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(listenerTestBound)
	for {
		conn, err := net.DialTimeout("tcp4", addr, 25*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("SIP listener %s was not ready: %v", addr, err)
		}
	}
}

func waitForTCPListenerRelease(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(listenerTestBound)
	for {
		listener, err := net.Listen("tcp4", addr)
		if err == nil {
			_ = listener.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("SIP listener %s was not released: %v", addr, err)
		}
	}
}

func waitForLog(t *testing.T, logs *synchronizedLogBuffer, predicate func(string) bool) {
	t.Helper()
	timer := time.NewTimer(listenerTestBound)
	defer timer.Stop()
	for {
		if predicate(logs.String()) {
			return
		}
		select {
		case <-logs.changed:
		case <-timer.C:
			t.Fatalf("expected listener log was not emitted:\n%s", logs.String())
		}
	}
}
