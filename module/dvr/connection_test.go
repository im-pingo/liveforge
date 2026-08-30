package dvr

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
)

type dvrBlockingAuthorizer struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newDVRBlockingAuthorizer() *dvrBlockingAuthorizer {
	return &dvrBlockingAuthorizer{entered: make(chan struct{}), release: make(chan struct{})}
}

func (a *dvrBlockingAuthorizer) Authorize(ctx context.Context, _ core.AuthorizationRequest) error {
	a.once.Do(func() { close(a.entered) })
	select {
	case <-a.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type dvrPipeListener struct {
	mu        sync.Mutex
	conn      net.Conn
	closed    chan struct{}
	closeOnce sync.Once
}

func newDVRPipeListener(conn net.Conn) *dvrPipeListener {
	return &dvrPipeListener{conn: conn, closed: make(chan struct{})}
}

func (l *dvrPipeListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	if l.conn != nil {
		conn := l.conn
		l.conn = nil
		l.mu.Unlock()
		return conn, nil
	}
	closed := l.closed
	l.mu.Unlock()
	<-closed
	return nil, net.ErrClosed
}

func (l *dvrPipeListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return nil
}

func (l *dvrPipeListener) Addr() net.Addr { return dvrPipeAddr("dvr-pipe") }

type dvrPipeAddr string

func (a dvrPipeAddr) Network() string { return string(a) }
func (a dvrPipeAddr) String() string  { return string(a) }

type observedDVRPipeConn struct {
	net.Conn
	closed           chan struct{}
	closeOnce        sync.Once
	maxWriteDeadline time.Duration
}

func newObservedDVRPipeConn(conn net.Conn, maxWriteDeadline time.Duration) *observedDVRPipeConn {
	return &observedDVRPipeConn{
		Conn:             conn,
		closed:           make(chan struct{}),
		maxWriteDeadline: maxWriteDeadline,
	}
}

func (c *observedDVRPipeConn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		err = c.Conn.Close()
		close(c.closed)
	})
	return err
}

func (c *observedDVRPipeConn) SetWriteDeadline(deadline time.Time) error {
	if c.maxWriteDeadline > 0 && !deadline.IsZero() {
		capped := time.Now().Add(c.maxWriteDeadline)
		if deadline.After(capped) {
			deadline = capped
		}
	}
	return c.Conn.SetWriteDeadline(deadline)
}

type dvrPipeServerHarness struct {
	client         net.Conn
	clientClosed   <-chan struct{}
	serverClosed   <-chan struct{}
	listenerClosed <-chan struct{}
	stop           func()
}

func TestHandleMediaRejectsWhenGlobalConnectionLimitIsFull(t *testing.T) {
	cfg := config.Defaults()
	cfg.Limits.MaxConnections = 1
	server := core.NewServer(cfg)
	if !server.AcquireConn() {
		t.Fatal("failed to occupy the only global connection")
	}
	defer server.ReleaseConn()

	m := NewModule()
	m.server = server
	req := httptest.NewRequest(http.MethodGet, "/dvr/live/camera.m3u8", nil)
	req.SetPathValue("app", "live")
	req.SetPathValue("resource", "camera.m3u8")
	response := httptest.NewRecorder()

	m.handleMedia(response, req)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
	if got := server.ConnectionCount(); got != 1 {
		t.Fatalf("connection count = %d, want occupied count 1", got)
	}
}

func TestDVRRequestReleasesExactlyOneConnectionSlotOnSuccess(t *testing.T) {
	m, server := newDVRConnectionTestModule(t, []byte("finite-segment"))
	authorizer := newDVRBlockingAuthorizer()
	server.SetAuthorizer(authorizer)
	pipe := startDVRPipeServer(t, newDVRHTTPServer(m), 0)
	defer pipe.stop()

	req := writeDVRPipeRequest(t, pipe.client, "/dvr/live/camera/seg_000000.ts")
	<-authorizer.entered
	waitForDVRConnectionCount(t, server, 2, time.Second)
	close(authorizer.release)

	response, err := http.ReadResponse(bufio.NewReader(pipe.client), req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || string(body) != "finite-segment" {
		t.Fatalf("status=%d body=%q", response.StatusCode, body)
	}
	waitForDVRConnectionCount(t, server, 1, time.Second)
}

func TestDVRRequestReleasesExactlyOneConnectionSlotOnError(t *testing.T) {
	m, server := newDVRConnectionTestModule(t, nil)
	authorizer := newDVRBlockingAuthorizer()
	server.SetAuthorizer(authorizer)
	pipe := startDVRPipeServer(t, newDVRHTTPServer(m), 0)
	defer pipe.stop()

	req := writeDVRPipeRequest(t, pipe.client, "/dvr/live/missing.m3u8")
	<-authorizer.entered
	waitForDVRConnectionCount(t, server, 2, time.Second)
	close(authorizer.release)

	response, err := http.ReadResponse(bufio.NewReader(pipe.client), req)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d, want %d", response.StatusCode, http.StatusNotFound)
	}
	waitForDVRConnectionCount(t, server, 1, time.Second)
}

func TestDVRRequestReleasesExactlyOneConnectionSlotOnClientCancellation(t *testing.T) {
	m, server := newDVRConnectionTestModule(t, []byte("finite-segment"))
	authorizer := newDVRBlockingAuthorizer()
	server.SetAuthorizer(authorizer)
	pipe := startDVRPipeServer(t, newDVRHTTPServer(m), 0)
	defer pipe.stop()

	writeDVRPipeRequest(t, pipe.client, "/dvr/live/camera/seg_000000.ts")
	<-authorizer.entered
	waitForDVRConnectionCount(t, server, 2, time.Second)
	if err := pipe.client.Close(); err != nil {
		t.Fatal(err)
	}
	waitForDVRConnectionCount(t, server, 1, time.Second)
}

func TestDVRStalledSegmentWriteReleasesConnectionSlotWithinWriteTimeout(t *testing.T) {
	m, server := newDVRConnectionTestModule(t, make([]byte, 64*1024))
	const effectiveWriteTimeout = 40 * time.Millisecond
	httpServer := newDVRHTTPServer(m)
	if httpServer.WriteTimeout != dvrResponseWriteTimeout {
		t.Errorf("production DVR write timeout = %v, want %v", httpServer.WriteTimeout, dvrResponseWriteTimeout)
	}
	handlerDone := make(chan struct{})
	realHandler := httpServer.Handler
	httpServer.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(handlerDone)
		realHandler.ServeHTTP(w, r)
	})
	connectionClosed := make(chan struct{})
	var connectionClosedOnce sync.Once
	httpServer.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			connectionClosedOnce.Do(func() { close(connectionClosed) })
		}
	}
	pipe := startDVRPipeServer(t, httpServer, effectiveWriteTimeout)
	defer pipe.stop()

	writeDVRPipeRequest(t, pipe.client, "/dvr/live/camera/seg_000000.ts")
	waitForDVRConnectionCount(t, server, 2, time.Second)
	started := time.Now()
	deadline := started.Add(10 * effectiveWriteTimeout)
	waitForDVRConnectionCount(t, server, 1, time.Until(deadline))
	waitForDVRSignalUntil(t, handlerDone, deadline, "DVR handler return")
	waitForDVRSignalUntil(t, connectionClosed, deadline, "http.Server StateClosed")
	waitForDVRSignalUntil(t, pipe.serverClosed, deadline, "server-side transport close")
	assertDVRSignalOpen(t, pipe.clientClosed, "unread peer")
	assertDVRSignalOpen(t, pipe.listenerClosed, "DVR server listener")
}

func newDVRConnectionTestModule(t *testing.T, segmentData []byte) (*Module, *core.Server) {
	t.Helper()
	cfg := config.Defaults()
	cfg.Limits.MaxConnections = 0
	cfg.DVR.Path = filepath.Join(t.TempDir(), "{stream_key}")
	server := core.NewServer(cfg)
	if !server.AcquireConn() {
		t.Fatal("failed to occupy baseline global connection slot")
	}
	t.Cleanup(func() {
		if got := server.ConnectionCount(); got != 1 {
			t.Errorf("connection count before baseline release = %d, want 1", got)
		}
		server.ReleaseConn()
	})

	m := NewModule()
	m.server = server
	if segmentData == nil {
		return m, server
	}
	stream, err := server.StreamHub().GetOrCreate("live/camera")
	if err != nil {
		t.Fatal(err)
	}
	session, err := NewSession("live/camera", stream, cfg.DVR, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := session.Close(); closeErr != nil {
			t.Errorf("close session: %v", closeErr)
		}
	})
	filename := "seg_000000.ts"
	segmentPath := filepath.Join(resolvePath(cfg.DVR.Path, stream.Key()), filename)
	if writeErr := os.WriteFile(segmentPath, segmentData, 0600); writeErr != nil {
		t.Fatal(writeErr)
	}
	info, err := os.Stat(segmentPath)
	if err != nil {
		t.Fatal(err)
	}
	session.Index().Add(Segment{
		SeqNum:    0,
		StartTime: info.ModTime(),
		Filename:  filename,
		Size:      info.Size(),
		DiskPath:  segmentPath,
	})
	m.sessions[stream.Key()] = session
	return m, server
}

func startDVRPipeServer(t *testing.T, httpServer *http.Server, maxWriteDeadline time.Duration) *dvrPipeServerHarness {
	t.Helper()
	serverPipe, clientPipe := net.Pipe()
	serverConn := newObservedDVRPipeConn(serverPipe, maxWriteDeadline)
	clientConn := newObservedDVRPipeConn(clientPipe, 0)
	listener := newDVRPipeListener(serverConn)
	serveDone := make(chan error, 1)
	go func() { serveDone <- httpServer.Serve(listener) }()
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			_ = clientConn.Close()
			_ = httpServer.Close()
			select {
			case err := <-serveDone:
				if err != nil && !errors.Is(err, http.ErrServerClosed) {
					t.Errorf("serve DVR pipe: %v", err)
				}
			case <-time.After(time.Second):
				t.Error("DVR pipe server did not stop")
			}
		})
	}
	return &dvrPipeServerHarness{
		client:         clientConn,
		clientClosed:   clientConn.closed,
		serverClosed:   serverConn.closed,
		listenerClosed: listener.closed,
		stop:           stop,
	}
}

func writeDVRPipeRequest(t *testing.T, conn net.Conn, target string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://dvr.test"+target, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Close = true
	if err := req.Write(conn); err != nil {
		t.Fatal(err)
	}
	return req
}

func waitForDVRConnectionCount(t *testing.T, server *core.Server, want int64, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if server.ConnectionCount() == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("connection count = %d, want %d", server.ConnectionCount(), want)
}

func waitForDVRSignalUntil(t *testing.T, signal <-chan struct{}, deadline time.Time, name string) {
	t.Helper()
	remaining := time.Until(deadline)
	if remaining <= 0 {
		t.Fatalf("%s did not complete before deadline", name)
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-signal:
	case <-timer.C:
		t.Fatalf("%s did not complete before deadline", name)
	}
}

func assertDVRSignalOpen(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
		t.Fatalf("%s closed before lifecycle assertions completed", name)
	default:
	}
}
