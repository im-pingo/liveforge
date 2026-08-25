package rtsp

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
)

func TestModuleInterface(t *testing.T) {
	m := NewModule()
	var _ core.Module = m

	if m.Name() != "rtsp" {
		t.Errorf("Name = %q, want %q", m.Name(), "rtsp")
	}
	if hooks := m.Hooks(); hooks != nil {
		t.Errorf("Hooks should be nil, got %v", hooks)
	}
}

func TestGenerateSessionID(t *testing.T) {
	id1 := generateSessionID()
	id2 := generateSessionID()
	if len(id1) != 16 { // 8 bytes = 16 hex chars
		t.Errorf("session ID length = %d, want 16", len(id1))
	}
	if id1 == id2 {
		t.Error("two session IDs should be different")
	}
}

func newShutdownTestModule(t *testing.T, drainTimeout time.Duration) (*Module, *core.Server) {
	t.Helper()
	cfg := &config.Config{
		Server: config.ServerConfig{DrainTimeout: drainTimeout},
		RTSP: config.RTSPConfig{
			Listen:       "127.0.0.1:0",
			RTPPortRange: []int{42300, 42319},
		},
		Stream: config.StreamConfig{RingBufferSize: 16},
	}
	server := core.NewServer(cfg)
	module := NewModule()
	if err := module.Init(server); err != nil {
		t.Fatal(err)
	}
	return module, server
}

func waitForRTSPConnections(t *testing.T, server *core.Server, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if server.ConnectionCount() == int64(want) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("RTSP connection count = %d, want %d", server.ConnectionCount(), want)
}

type closeOrderingListener struct {
	release    chan struct{}
	returned   chan struct{}
	closeOnce  sync.Once
	returnOnce sync.Once
}

func newCloseOrderingListener() *closeOrderingListener {
	return &closeOrderingListener{release: make(chan struct{}), returned: make(chan struct{})}
}

func (l *closeOrderingListener) Accept() (net.Conn, error) {
	<-l.release
	l.returnOnce.Do(func() { close(l.returned) })
	return nil, net.ErrClosed
}

func (l *closeOrderingListener) Close() error {
	l.closeOnce.Do(func() { close(l.release) })
	<-l.returned
	time.Sleep(20 * time.Millisecond)
	return nil
}

func (*closeOrderingListener) Addr() net.Addr { return &net.TCPAddr{} }

func TestModuleCloseDoesNotLogListenerClosureAsAcceptError(t *testing.T) {
	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	m := NewModule()
	m.listener = newCloseOrderingListener()
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.acceptLoop()
	}()

	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "accept error") {
		t.Fatalf("normal listener closure logged as an error:\n%s", logs.String())
	}
}

func TestModuleCloseClosesAcceptedConnectionWaitsForHandlerAndIsIdempotent(t *testing.T) {
	module, server := newShutdownTestModule(t, time.Second)
	conn, err := net.Dial("tcp", module.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	waitForRTSPConnections(t, server, 1)

	if err := module.Close(); err != nil {
		t.Fatal(err)
	}
	if err := module.Close(); err != nil {
		t.Fatalf("second Close failed: %v", err)
	}
	waitForRTSPConnections(t, server, 0)
	if got := module.connectionCount(); got != 0 {
		t.Fatalf("tracked accepted connections after Close = %d", got)
	}
	if _, err := io.WriteString(conn, "OPTIONS * RTSP/1.0\r\nCSeq: 1\r\n\r\n"); err == nil {
		_ = conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		if _, readErr := bufio.NewReader(conn).ReadString('\n'); readErr == nil {
			t.Fatal("accepted connection remained usable after module Close")
		}
	}
}

func TestModuleClosePreventsPostSnapshotAnnounceMutation(t *testing.T) {
	module, server := newShutdownTestModule(t, time.Second)
	conn, err := net.Dial("tcp", module.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	waitForRTSPConnections(t, server, 1)

	if err := module.Close(); err != nil {
		t.Fatal(err)
	}
	body := "v=0\r\no=- 0 0 IN IP4 0.0.0.0\r\ns=test\r\nt=0 0\r\nm=video 0 RTP/AVP 96\r\na=rtpmap:96 H264/90000\r\n"
	request := fmt.Sprintf("ANNOUNCE rtsp://localhost/live/after-close RTSP/1.0\r\nCSeq: 1\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
	_, _ = io.WriteString(conn, request)
	time.Sleep(20 * time.Millisecond)
	if stream, ok := server.StreamHub().Find("live/after-close"); ok && stream.Publisher() != nil {
		t.Fatal("post-close request installed a publisher after cleanup snapshot")
	}
}

func TestModuleCloseBoundsBlockedAcceptedHandlerByDrainTimeout(t *testing.T) {
	module, server := newShutdownTestModule(t, 20*time.Millisecond)
	entered := make(chan struct{})
	release := make(chan struct{})
	server.GetEventBus().Register(core.HookRegistration{
		Event: core.EventPublish,
		Mode:  core.HookSync,
		Handler: func(*core.EventContext) error {
			close(entered)
			<-release
			return nil
		},
	})
	conn, err := net.Dial("tcp", module.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	body := "v=0\r\no=- 0 0 IN IP4 0.0.0.0\r\ns=test\r\nt=0 0\r\nm=video 0 RTP/AVP 96\r\na=rtpmap:96 H264/90000\r\n"
	request := fmt.Sprintf("ANNOUNCE rtsp://localhost/live/blocked RTSP/1.0\r\nCSeq: 1\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
	if _, err := io.WriteString(conn, request); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("accepted handler did not enter publish authorization")
	}

	started := time.Now()
	err = module.Close()
	if err == nil || !errors.Is(err, errRTSPDrainTimeout) {
		t.Fatalf("Close error = %v, want drain timeout", err)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("Close exceeded drain policy: %v", elapsed)
	}
	close(release)
	waitForRTSPConnections(t, server, 0)
}
