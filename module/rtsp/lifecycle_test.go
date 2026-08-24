package rtsp

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/portalloc"
)

func TestRTSPPublishLifecycleStopWaitsForBlockedStart(t *testing.T) {
	m, server := newShutdownTestModule(t, time.Second)
	defer m.Close()
	startEntered := make(chan *core.EventContext, 1)
	releaseStart := make(chan struct{})
	stopRan := make(chan *core.EventContext, 1)
	server.GetEventBus().Register(core.HookRegistration{
		Event: core.EventPublish,
		Mode:  core.HookAsync,
		Handler: func(ctx *core.EventContext) error {
			startEntered <- ctx
			<-releaseStart
			return nil
		},
	})
	server.GetEventBus().Register(core.HookRegistration{
		Event: core.EventPublishStop,
		Mode:  core.HookAsync,
		Handler: func(ctx *core.EventContext) error {
			stopRan <- ctx
			return nil
		},
	})

	conn, err := net.Dial("tcp", m.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	body := "v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\ns=test\r\nt=0 0\r\nm=video 0 RTP/AVP 96\r\na=rtpmap:96 H264/90000\r\n"
	request := fmt.Sprintf("ANNOUNCE rtsp://localhost/live/ordered RTSP/1.0\r\nCSeq: 1\r\nContent-Type: application/sdp\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
	if _, err := io.WriteString(conn, request); err != nil {
		t.Fatal(err)
	}
	readRTSPTestResponse(t, reader, 200)
	start := receiveRTSPLifecycle(t, startEntered, "publish start")
	if start.PublisherID == "" {
		t.Fatal("RTSP publish start omitted publisher generation")
	}

	if _, err := io.WriteString(conn, "TEARDOWN rtsp://localhost/live/ordered RTSP/1.0\r\nCSeq: 2\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	readRTSPTestResponse(t, reader, 200)
	select {
	case <-stopRan:
		t.Fatal("RTSP publish stop overtook blocked start")
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseStart)
	stop := receiveRTSPLifecycle(t, stopRan, "publish stop")
	if stop.PublisherID != start.PublisherID {
		t.Fatalf("RTSP publish generation = %q then %q", start.PublisherID, stop.PublisherID)
	}
}

func TestRTSPSubscribeLifecycleStopWaitsForBlockedStart(t *testing.T) {
	m, server := newShutdownTestModule(t, time.Second)
	defer m.Close()
	stream, err := server.StreamHub().GetOrCreate("live/source")
	if err != nil {
		t.Fatal(err)
	}
	info := &avframe.MediaInfo{VideoCodec: avframe.CodecH264}
	pub, err := NewRTSPPublisher("source-generation", info, stream, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.SetPublisher(pub); err != nil {
		t.Fatal(err)
	}
	startEntered := make(chan *core.EventContext, 1)
	releaseStart := make(chan struct{})
	stopRan := make(chan *core.EventContext, 1)
	server.GetEventBus().Register(core.HookRegistration{
		Event: core.EventSubscribe,
		Mode:  core.HookAsync,
		Handler: func(ctx *core.EventContext) error {
			startEntered <- ctx
			<-releaseStart
			return nil
		},
	})
	server.GetEventBus().Register(core.HookRegistration{
		Event: core.EventSubscribeStop,
		Mode:  core.HookAsync,
		Handler: func(ctx *core.EventContext) error {
			stopRan <- ctx
			return nil
		},
	})

	conn, err := net.Dial("tcp", m.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	if _, err := io.WriteString(conn, "DESCRIBE rtsp://localhost/live/source RTSP/1.0\r\nCSeq: 1\r\nAccept: application/sdp\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	readRTSPTestResponse(t, reader, 200)
	if _, err := io.WriteString(conn, "SETUP rtsp://localhost/live/source/trackID=0 RTSP/1.0\r\nCSeq: 2\r\nTransport: RTP/AVP/TCP;unicast;interleaved=0-1\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	readRTSPTestResponse(t, reader, 200)
	if _, err := io.WriteString(conn, "PLAY rtsp://localhost/live/source RTSP/1.0\r\nCSeq: 3\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	readRTSPTestResponse(t, reader, 200)
	start := receiveRTSPLifecycle(t, startEntered, "subscribe start")
	if start.SubscriberID == "" {
		t.Fatal("RTSP subscribe start omitted subscriber generation")
	}

	m.mu.Lock()
	var session *RTSPSession
	for _, candidate := range m.sessions {
		session = candidate
		break
	}
	m.mu.Unlock()
	if session == nil || !m.cleanupSession(session) {
		t.Fatal("RTSP subscriber session cleanup did not run")
	}
	select {
	case <-stopRan:
		t.Fatal("RTSP subscribe stop overtook blocked start")
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseStart)
	stop := receiveRTSPLifecycle(t, stopRan, "subscribe stop")
	if stop.SubscriberID != start.SubscriberID {
		t.Fatalf("RTSP subscriber generation = %q then %q", start.SubscriberID, stop.SubscriberID)
	}
	stream.WriteFrame(&avframe.AVFrame{MediaType: avframe.MediaTypeVideo, FrameType: avframe.FrameTypeKeyframe, Payload: []byte{0x65}})
}

func readRTSPTestResponse(t *testing.T, reader *bufio.Reader, wantStatus int) {
	t.Helper()
	status, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status, fmt.Sprintf(" %d ", wantStatus)) {
		t.Fatalf("RTSP status = %q, want %d", strings.TrimSpace(status), wantStatus)
	}
	contentLength := 0
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
		name, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if ok && strings.EqualFold(name, "Content-Length") {
			contentLength, err = strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if contentLength > 0 {
		if _, err := io.CopyN(io.Discard, reader, int64(contentLength)); err != nil {
			t.Fatal(err)
		}
	}
}

func receiveRTSPLifecycle(t *testing.T, events <-chan *core.EventContext, name string) *core.EventContext {
	t.Helper()
	select {
	case ctx := <-events:
		return ctx
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for RTSP %s", name)
		return nil
	}
}

func TestCleanupSessionEmitsOneGenerationTaggedPublishStop(t *testing.T) {
	server := core.NewServer(&config.Config{Stream: config.StreamConfig{RingBufferSize: 16}})
	stream, err := server.StreamHub().GetOrCreate("live/cleanup")
	if err != nil {
		t.Fatal(err)
	}
	pub := &RTSPPublisher{id: "rtsp-generation-7", done: make(chan struct{})}
	if err := stream.SetPublisher(pub); err != nil {
		t.Fatal(err)
	}
	session := NewRTSPSession("session-7", "live/cleanup")
	session.Publisher = pub
	session.Stream = stream
	session.State = StateRecording
	session.RemoteAddr = "127.0.0.1:9000"
	if !session.MarkPublished() {
		t.Fatal("active RTSP session refused publish lifecycle mark")
	}
	m := NewModule()
	m.server = server
	m.sessions[session.ID] = session

	stops := make(chan *core.EventContext, 2)
	server.GetEventBus().Register(core.HookRegistration{
		Event: core.EventPublishStop,
		Mode:  core.HookAsync,
		Handler: func(ctx *core.EventContext) error {
			stops <- ctx
			return nil
		},
	})

	if !m.cleanupSession(session) {
		t.Fatal("first cleanup did not own RTSP termination")
	}
	if m.cleanupSession(session) {
		t.Fatal("repeated cleanup owned RTSP termination")
	}

	select {
	case stop := <-stops:
		if stop.PublisherID != pub.ID() {
			t.Fatalf("stop publisher ID = %q, want %q", stop.PublisherID, pub.ID())
		}
	case <-time.After(time.Second):
		t.Fatal("publish stop event not emitted")
	}
	select {
	case stop := <-stops:
		t.Fatalf("duplicate publish stop event: %#v", stop)
	case <-time.After(50 * time.Millisecond):
	}
	if stream.Publisher() != nil {
		t.Fatal("closed RTSP publisher remains installed")
	}
	m.mu.Lock()
	_, exists := m.sessions[session.ID]
	m.mu.Unlock()
	if exists {
		t.Fatal("closed RTSP session remains registered")
	}
}

func TestInterleavedActivityRefreshesSession(t *testing.T) {
	m := NewModule()
	session := NewRTSPSession("tcp", "live/tcp")
	old := time.Now().Add(-time.Hour)
	session.mu.Lock()
	session.lastTouch = old
	session.mu.Unlock()

	m.processInterleaved(session, 1, []byte{0x00})

	session.mu.Lock()
	touched := session.lastTouch
	session.mu.Unlock()
	if !touched.After(old) {
		t.Fatalf("interleaved activity did not refresh session: %v", touched)
	}
}

func TestUDPPublishActivityRefreshesSession(t *testing.T) {
	ports, err := portalloc.New(42200, 42201)
	if err != nil {
		t.Fatal(err)
	}
	transport, err := NewUDPTransport(ports)
	if err != nil {
		t.Fatal(err)
	}
	defer transport.Close()
	session := NewRTSPSession("udp", "live/udp")
	old := time.Now().Add(-time.Hour)
	session.mu.Lock()
	session.lastTouch = old
	session.mu.Unlock()
	m := NewModule()
	go m.udpPublishLoop(transport, session)

	rtpPort, _ := transport.ServerPorts()
	client, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: rtpPort})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Write(make([]byte, 12)); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(time.Second)
	for {
		session.mu.Lock()
		touched := session.lastTouch
		session.mu.Unlock()
		if touched.After(old) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("successful UDP RTP read did not refresh session")
		}
		time.Sleep(time.Millisecond)
	}
}
