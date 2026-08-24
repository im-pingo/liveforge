package rtsp

import (
	"net"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/portalloc"
)

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
