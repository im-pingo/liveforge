package cluster

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/muxer/ps"
	"github.com/im-pingo/liveforge/pkg/portalloc"
	"github.com/im-pingo/liveforge/pkg/sdp"
	pionrtp "github.com/pion/rtp/v2"
)

type signalingFixture struct {
	server    *core.Server
	ports     *portalloc.PortAllocator
	push      http.HandlerFunc
	pull      http.HandlerFunc
	close     func() error
	pushRelay func(context.Context, string, *core.Stream) error
	pullRelay func(context.Context, string, *core.Stream) error
	port      int
}

func newSignalingFixture(t *testing.T, protocol string) *signalingFixture {
	t.Helper()
	cfg := config.Defaults()
	cfg.Stream.IdleTimeout = time.Hour
	cfg.Stream.NoPublisherTimeout = time.Hour
	f := &signalingFixture{server: core.NewServer(cfg)}
	// Let the OS find a candidate, then reserve both neighbors for GB.
	for attempts := 0; attempts < 100; attempts++ {
		probe, err := net.ListenUDP("udp", &net.UDPAddr{})
		if err != nil {
			t.Fatal(err)
		}
		port := probe.LocalAddr().(*net.UDPAddr).Port
		probe.Close()
		if port%2 != 0 || port == 65535 {
			continue
		}
		pa, err := portalloc.New(port, port+1)
		if err != nil {
			t.Fatal(err)
		}
		pair, err := pa.AllocateBoundUDPPair("udp", nil)
		if err != nil {
			continue
		}
		pair.RTPConn.Close()
		pair.RTCPConn.Close()
		pa.Free(port, port+1)
		f.ports, f.port = pa, port
		break
	}
	if f.ports == nil {
		t.Fatal("no available UDP pair")
	}
	if protocol == "rtp" {
		tr := &RTPTransport{cfg: defaultClusterRTPConfig(), ports: f.ports, hub: f.server.StreamHub(), server: f.server}
		tr.cfg.Timeout = 200 * time.Millisecond
		f.push, f.pull, f.close = tr.handleSignalingPush, tr.handleSignalingPull, tr.Close
		f.pushRelay, f.pullRelay = tr.Push, tr.Pull
	} else {
		tr := &GBTransport{cfg: config.ClusterGBConfig{Timeout: 200 * time.Millisecond}, ports: f.ports, hub: f.server.StreamHub(), server: f.server}
		f.push, f.pull, f.close = tr.handlePushSignal, tr.handlePullSignal, tr.Close
		f.pushRelay, f.pullRelay = tr.Push, tr.Pull
	}
	t.Cleanup(func() {
		if closeErr := f.close(); closeErr != nil {
			t.Errorf("close transport: %v", closeErr)
		}
		for _, key := range f.server.StreamHub().Keys() {
			f.server.StreamHub().Remove(key)
		}
	})
	return f
}

func TestClusterPushRejectsStreamCapacityWithoutLeakingPorts(t *testing.T) {
	for _, protocol := range []string{"rtp", "gb"} {
		t.Run(protocol, func(t *testing.T) {
			f := newSignalingFixture(t, protocol)
			f.server.StreamHub().UpdatePolicy(f.server.Config().Stream, config.LimitsConfig{MaxStreams: 1})
			if _, err := f.server.StreamHub().GetOrCreate("live/full"); err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			f.push(w, signalingRequest("live/rejected"))
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("capacity status = %d, want 503", w.Code)
			}
			if f.server.StreamHub().Count() != 1 {
				t.Fatal("capacity rejection created a stream")
			}
			pair, err := f.ports.AllocateBoundUDPPair("udp", nil)
			if err != nil {
				t.Fatalf("capacity rejection leaked ports: %v", err)
			}
			pair.RTPConn.Close()
			pair.RTCPConn.Close()
			f.ports.Free(pair.RTPPort, pair.RTCPPort)
		})
	}
}

func TestClusterPushAuthorizationFailureRollsBackSocket(t *testing.T) {
	for _, protocol := range []string{"rtp", "gb"} {
		t.Run(protocol, func(t *testing.T) {
			f := newSignalingFixture(t, protocol)
			f.server.GetEventBus().Register(core.HookRegistration{Event: core.EventPublish, Mode: core.HookSync, Handler: func(*core.EventContext) error { return errors.New("denied") }})
			w := httptest.NewRecorder()
			f.push(w, signalingRequest("live/denied"))
			if w.Code != http.StatusForbidden {
				t.Fatalf("authorization status = %d, want 403", w.Code)
			}
			if f.server.StreamHub().Count() != 0 {
				t.Fatal("denied publisher created a stream")
			}
			pair, err := f.ports.AllocateBoundUDPPair("udp", nil)
			if err != nil {
				t.Fatalf("authorization rejection leaked ports: %v", err)
			}
			pair.RTPConn.Close()
			pair.RTCPConn.Close()
			f.ports.Free(pair.RTPPort, pair.RTCPPort)
		})
	}
}

func TestClusterTransportCloseCancelsOutgoingSignaling(t *testing.T) {
	for _, protocol := range []string{"rtp", "gb"} {
		for _, direction := range []string{"push", "pull"} {
			t.Run(protocol+"/"+direction, func(t *testing.T) {
				f := newSignalingFixture(t, protocol)
				entered, release := make(chan struct{}), make(chan struct{})
				peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					close(entered)
					select {
					case <-r.Context().Done():
					case <-release:
					}
				}))
				defer peer.Close()
				defer close(release)
				stream, _ := f.server.StreamHub().GetOrCreate("live/outgoing")
				scheme := "rtp"
				if protocol == "gb" {
					scheme = "gb28181"
				}
				relay := f.pullRelay
				if direction == "push" {
					relay = f.pushRelay
					if err := stream.SetPublisher(newOriginPublisher("test", stream.Key(), &avframe.MediaInfo{VideoCodec: avframe.CodecH264})); err != nil {
						t.Fatal(err)
					}
				}
				result := make(chan error, 1)
				go func() {
					result <- relay(context.Background(), scheme+"://"+peer.Listener.Addr().String()+"/live/outgoing", stream)
				}()
				select {
				case <-entered:
				case <-time.After(time.Second):
					t.Fatal("signaling did not start")
				}
				if err := f.close(); err != nil {
					t.Fatal(err)
				}
				select {
				case <-result:
				case <-time.After(time.Second):
					t.Fatal("Close left outgoing signaling blocked")
				}
			})
		}
	}
}

func TestClusterGBPushBoundsUnterminatedPS(t *testing.T) {
	f := newSignalingFixture(t, "gb")
	transport := &GBTransport{cfg: config.ClusterGBConfig{Timeout: time.Hour}, ports: f.ports, hub: f.server.StreamHub(), server: f.server}
	f.close = transport.Close
	w := httptest.NewRecorder()
	transport.handlePushSignal(w, signalingRequest("live/oversized-ps"))
	if w.Code != http.StatusOK {
		t.Fatalf("push status = %d", w.Code)
	}
	stream, _ := f.server.StreamHub().Find("live/oversized-ps")
	ended := stream.StartupSnapshot().GenerationDone
	port, err := strconv.Atoi(w.Body.String())
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for sequence := 0; sequence < 8192; sequence++ {
		packet := &pionrtp.Packet{Header: pionrtp.Header{Version: 2, PayloadType: 96, SequenceNumber: uint16(sequence), SSRC: 7}, Payload: bytes.Repeat([]byte{0x01}, 1400)}
		data, err := packet.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Write(data); err != nil {
			break
		}
		select {
		case <-ended:
			return
		default:
		}
		if sequence%16 == 0 {
			time.Sleep(time.Millisecond)
		}
	}
	select {
	case <-ended:
	case <-time.After(time.Second):
		t.Fatal("unterminated PS fragments did not terminate the publisher at the memory bound")
	}
}

func signalingRequest(key string) *http.Request {
	offer := sdp.BuildFromMediaInfo(&avframe.MediaInfo{VideoCodec: avframe.CodecH264}, "", "127.0.0.1")
	for _, media := range offer.Media {
		media.Port = 19000
	}
	r := httptest.NewRequest(http.MethodPost, "/?stream="+key+"&port=19000", bytes.NewReader(offer.Marshal()))
	r.RemoteAddr = "127.0.0.1:19001"
	return r
}

func TestClusterPushAdmitsNewPublisherBeforeSuccess(t *testing.T) {
	for _, protocol := range []string{"rtp", "gb"} {
		t.Run(protocol, func(t *testing.T) {
			f := newSignalingFixture(t, protocol)
			w := httptest.NewRecorder()
			f.push(w, signalingRequest("live/new"))
			if w.Code != http.StatusOK {
				t.Fatalf("push status = %d, body = %s", w.Code, w.Body.String())
			}
			stream, ok := f.server.StreamHub().Find("live/new")
			if !ok || stream.Publisher() == nil {
				t.Fatal("success returned before publisher admission")
			}
			port := f.port
			if protocol == "gb" {
				var err error
				port, err = strconv.Atoi(w.Body.String())
				if err != nil {
					t.Fatal(err)
				}
			}
			probe, err := net.ListenUDP("udp", &net.UDPAddr{Port: port})
			if err == nil {
				probe.Close()
				t.Fatal("success returned before UDP listener was bound")
			}
		})
	}
}

func TestClusterPushRejectsOccupiedPortsBeforeSuccess(t *testing.T) {
	for _, protocol := range []string{"rtp", "gb"} {
		t.Run(protocol, func(t *testing.T) {
			f := newSignalingFixture(t, protocol)
			stream, err := f.server.StreamHub().GetOrCreate("live/occupied")
			if err != nil {
				t.Fatal(err)
			}
			for _, port := range []int{f.port, f.port + 1} {
				conn, err := net.ListenUDP("udp", &net.UDPAddr{Port: port})
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
			}
			w := httptest.NewRecorder()
			f.push(w, signalingRequest(stream.Key()))
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("occupied-port status = %d, want 503", w.Code)
			}
			if stream.Publisher() != nil {
				t.Fatal("failed binding installed a publisher")
			}
		})
	}
}

func TestClusterPushRejectsPublisherConflict(t *testing.T) {
	for _, protocol := range []string{"rtp", "gb"} {
		t.Run(protocol, func(t *testing.T) {
			f := newSignalingFixture(t, protocol)
			stream, err := f.server.StreamHub().GetOrCreate("live/existing")
			if err != nil {
				t.Fatal(err)
			}
			pub := newOriginPublisher("test", stream.Key(), &avframe.MediaInfo{VideoCodec: avframe.CodecH264})
			if publishErr := stream.SetPublisher(pub); publishErr != nil {
				t.Fatal(publishErr)
			}
			w := httptest.NewRecorder()
			f.push(w, signalingRequest(stream.Key()))
			if w.Code != http.StatusConflict {
				t.Fatalf("publisher-conflict status = %d, want 409", w.Code)
			}
			if stream.Publisher() != pub {
				t.Fatal("conflict changed the existing publisher")
			}
			pair, err := f.ports.AllocateBoundUDPPair("udp", nil)
			if err != nil {
				t.Fatalf("failed admission leaked ports: %v", err)
			}
			pair.RTPConn.Close()
			pair.RTCPConn.Close()
			f.ports.Free(pair.RTPPort, pair.RTCPPort)
		})
	}
}

func TestClusterFailedPushReleasesNewStreamCapacity(t *testing.T) {
	for _, protocol := range []string{"rtp", "gb"} {
		t.Run(protocol, func(t *testing.T) {
			f := newSignalingFixture(t, protocol)
			streamConfig := f.server.Config().Stream
			streamConfig.NoPublisherTimeout, streamConfig.IdleTimeout = 0, 0
			f.server.StreamHub().UpdatePolicy(streamConfig, config.LimitsConfig{MaxStreams: 1})
			f.push(failedSignalingResponse{httptest.NewRecorder()}, signalingRequest("live/rejected"))
			if got := f.server.StreamHub().Count(); got != 0 {
				t.Fatalf("failed push retains %d stream slots with cleanup timers disabled", got)
			}
			w := httptest.NewRecorder()
			f.push(w, signalingRequest("live/next"))
			if w.Code != http.StatusOK {
				t.Fatalf("next push status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestClusterSignalingBoundsRequestBody(t *testing.T) {
	for _, protocol := range []string{"rtp", "gb"} {
		t.Run(protocol, func(t *testing.T) {
			f := newSignalingFixture(t, protocol)
			stream, _ := f.server.StreamHub().GetOrCreate("live/large")
			if err := stream.SetPublisher(newOriginPublisher("test", stream.Key(), &avframe.MediaInfo{VideoCodec: avframe.CodecH264})); err != nil {
				t.Fatal(err)
			}
			for _, handler := range []http.HandlerFunc{f.push, f.pull} {
				r := httptest.NewRequest(http.MethodPost, "/?stream=live/large&port=19000", bytes.NewReader(bytes.Repeat([]byte("x"), (64<<10)+1)))
				w := httptest.NewRecorder()
				handler(w, r)
				if w.Code != http.StatusRequestEntityTooLarge {
					t.Errorf("oversized request status = %d, want 413", w.Code)
				}
			}
		})
	}
}

func TestClusterTransportCloseJoinsIncomingPublisherLifecycle(t *testing.T) {
	for _, protocol := range []string{"rtp", "gb"} {
		t.Run(protocol, func(t *testing.T) {
			f := newSignalingFixture(t, protocol)
			starts, stops := make(chan core.EventContext, 1), make(chan core.EventContext, 1)
			for event, output := range map[core.EventType]chan core.EventContext{core.EventPublish: starts, core.EventPublishStop: stops} {
				f.server.GetEventBus().Register(core.HookRegistration{Event: event, Mode: core.HookAsync, Consumer: "recording-test", Handler: func(ctx *core.EventContext) error { output <- *ctx; return nil }})
			}
			if _, createErr := f.server.StreamHub().GetOrCreate("live/close"); createErr != nil {
				t.Fatal(createErr)
			}
			w := httptest.NewRecorder()
			f.push(w, signalingRequest("live/close"))
			if w.Code != http.StatusOK {
				t.Fatalf("push = %d: %s", w.Code, w.Body.String())
			}
			if err := f.close(); err != nil {
				t.Fatal(err)
			}
			stream, _ := f.server.StreamHub().Find("live/close")
			if stream.Publisher() != nil {
				t.Error("Close returned with an incoming publisher still active")
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := f.server.GetEventBus().Drain(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case start := <-starts:
				select {
				case stop := <-stops:
					if start.PublisherGeneration == 0 || start.StreamInstanceID == 0 || fmt.Sprint(start) != fmt.Sprint(stop) {
						t.Fatalf("lifecycle mismatch: start=%+v stop=%+v", start, stop)
					}
				default:
					t.Error("missing terminal publish event")
				}
			default:
				t.Error("missing publish-start event")
			}
			pair, err := f.ports.AllocateBoundUDPPair("udp", nil)
			if err != nil {
				t.Fatalf("Close leaked UDP resources: %v", err)
			}
			pair.RTPConn.Close()
			pair.RTCPConn.Close()
			f.ports.Free(pair.RTPPort, pair.RTCPPort)
			w = httptest.NewRecorder()
			f.push(w, signalingRequest("live/after-close"))
			if w.Code != http.StatusServiceUnavailable {
				t.Errorf("closed transport admission = %d, want 503", w.Code)
			}
		})
	}
}

func TestClusterPullSubscriberAdmissionAndRelease(t *testing.T) {
	for _, protocol := range []string{"rtp", "gb"} {
		t.Run(protocol, func(t *testing.T) {
			f := newSignalingFixture(t, protocol)
			stream, _ := f.server.StreamHub().GetOrCreate("live/subscribers")
			stream.UpdatePolicy(f.server.Config().Stream, config.LimitsConfig{MaxSubscribersPerStream: 1})
			if err := stream.SetPublisher(newOriginPublisher("test", stream.Key(), &avframe.MediaInfo{VideoCodec: avframe.CodecH264})); err != nil {
				t.Fatal(err)
			}
			release, err := stream.AddSubscriberForGeneration("test", stream.StartupSnapshot().Generation)
			if err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			f.pull(w, signalingRequest(stream.Key()))
			if w.Code != http.StatusServiceUnavailable {
				t.Errorf("full subscriber admission = %d, want 503", w.Code)
			}
			release()
			w = httptest.NewRecorder()
			f.pull(w, signalingRequest(stream.Key()))
			if w.Code != http.StatusOK || stream.TotalSubscribers() != 1 {
				t.Errorf("admission status=%d subscribers=%d", w.Code, stream.TotalSubscribers())
			}
			if closeErr := f.close(); closeErr != nil {
				t.Fatal(closeErr)
			}
			if stream.TotalSubscribers() != 0 {
				t.Fatal("Close retained relay subscriber")
			}
		})
	}
}

func TestClusterOutgoingPullBindsBeforeSignalAndJoinsLifecycle(t *testing.T) {
	for _, protocol := range []string{"rtp", "gb"} {
		t.Run(protocol, func(t *testing.T) {
			f := newSignalingFixture(t, protocol)
			starts, stops := make(chan core.EventContext, 1), make(chan core.EventContext, 1)
			for event, output := range map[core.EventType]chan core.EventContext{core.EventPublish: starts, core.EventPublishStop: stops} {
				f.server.GetEventBus().Register(core.HookRegistration{Event: event, Mode: core.HookAsync, Consumer: "recording-test", Handler: func(ctx *core.EventContext) error { output <- *ctx; return nil }})
			}
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				probe, err := net.ListenUDP("udp", &net.UDPAddr{Port: f.port})
				if err == nil {
					probe.Close()
					t.Error("receive port was not bound before peer signaling")
				}
				if r.URL.Query().Get("stream") != "live/outgoing" {
					t.Errorf("signaled unexpected stream key: %q", r.URL.Query().Get("stream"))
				}
				if protocol == "rtp" {
					if _, writeErr := w.Write(sdp.BuildFromMediaInfo(&avframe.MediaInfo{VideoCodec: avframe.CodecH264}, "", "127.0.0.1").Marshal()); writeErr != nil {
						t.Errorf("write peer SDP: %v", writeErr)
					}
				}
			}))
			defer peer.Close()
			stream, _ := f.server.StreamHub().GetOrCreate("live/outgoing")
			result := make(chan error, 1)
			go func() {
				result <- f.pullRelay(context.Background(), map[string]string{"rtp": "rtp", "gb": "gb28181"}[protocol]+"://"+peer.Listener.Addr().String()+"/live/outgoing", stream)
			}()
			select {
			case <-starts:
			case <-time.After(time.Second):
				t.Error("outgoing pull did not emit publish lifecycle")
			}
			if closeErr := f.close(); closeErr != nil {
				t.Fatal(closeErr)
			}
			select {
			case err := <-result:
				if err != nil && !strings.Contains(err.Error(), "timeout") {
					t.Errorf("pull cleanup: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("Close did not join outgoing pull")
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := f.server.GetEventBus().Drain(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case <-stops:
			default:
				t.Error("outgoing pull did not emit publish-stop lifecycle")
			}
			if stream.Publisher() != nil {
				t.Fatal("Close retained outgoing pull publisher")
			}
		})
	}
}

func TestClusterGBReceivedAudioRemainsImmutable(t *testing.T) {
	f := newSignalingFixture(t, "gb")
	w := httptest.NewRecorder()
	f.push(w, signalingRequest("live/audio"))
	if w.Code != http.StatusOK {
		t.Fatalf("push = %d", w.Code)
	}
	port, err := strconv.Atoi(w.Body.String())
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	stream, _ := f.server.StreamHub().Find("live/audio")
	reader := stream.RingBuffer().NewReaderAt(stream.StartupSnapshot().LiveCursor)
	muxer := ps.NewMuxer()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var first *avframe.AVFrame
	for i, value := range []byte{0x55, 0xaa} {
		payload, err := muxer.Pack(avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecG711A, avframe.FrameTypeKeyframe, int64(i*20), int64(i*20), bytes.Repeat([]byte{value}, 160)))
		if err != nil {
			t.Fatal(err)
		}
		packet := &pionrtp.Packet{Header: pionrtp.Header{Version: 2, PayloadType: 96, SequenceNumber: uint16(i), Marker: true}, Payload: payload}
		raw, err := packet.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		if _, writeErr := conn.Write(raw); writeErr != nil {
			t.Fatal(writeErr)
		}
		frame, ok, err := core.ReadFrameContext(ctx, reader)
		if err != nil || !ok || frame == nil {
			t.Fatalf("audio receive: ok=%v error=%v", ok, err)
		}
		if i == 0 {
			first = frame
		}
	}
	if !bytes.Equal(first.Payload, bytes.Repeat([]byte{0x55}, 160)) {
		t.Fatal("later PS assembly changed retained source audio")
	}
}

type failedSignalingResponse struct{ *httptest.ResponseRecorder }

func (failedSignalingResponse) Write([]byte) (int, error) { return 0, errors.New("peer disconnected") }

func TestClusterPushResponseFailureReleasesPublisher(t *testing.T) {
	for _, protocol := range []string{"rtp", "gb"} {
		t.Run(protocol, func(t *testing.T) {
			f := newSignalingFixture(t, protocol)
			f.push(failedSignalingResponse{httptest.NewRecorder()}, signalingRequest("live/disconnected"))
			if stream, ok := f.server.StreamHub().Find("live/disconnected"); ok {
				select {
				case <-stream.StartupSnapshot().GenerationDone:
				case <-time.After(100 * time.Millisecond):
					t.Fatal("failed signaling response left publisher active")
				}
			}
		})
	}
}

func TestClusterRTPPullFromPeerDeliversAdvertisedMedia(t *testing.T) {
	for _, audio := range []bool{false, true} {
		t.Run(fmt.Sprintf("audio=%t", audio), func(t *testing.T) {
			peer, local := newSignalingFixture(t, "rtp"), newSignalingFixture(t, "rtp")
			info := &avframe.MediaInfo{VideoCodec: avframe.CodecH264}
			frame := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 0, 0, []byte{0, 0, 0, 3, 0x65, 0x88, 0x84})
			if audio {
				info = &avframe.MediaInfo{AudioCodec: avframe.CodecG711A, SampleRate: 8000, Channels: 1}
				frame = avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecG711A, avframe.FrameTypeKeyframe, 0, 0, bytes.Repeat([]byte{0x55}, 160))
			}
			source, _ := peer.server.StreamHub().GetOrCreate("live/peer")
			pub := newOriginPublisher("test", source.Key(), info)
			if err := source.SetPublisher(pub); err != nil {
				t.Fatal(err)
			}
			ready := make(chan struct{})
			local.server.GetEventBus().Register(core.HookRegistration{Event: core.EventPublish, Mode: core.HookAsync, Handler: func(*core.EventContext) error { close(ready); return nil }})
			remote := httptest.NewServer(peer.pull)
			defer remote.Close()
			destination, _ := local.server.StreamHub().GetOrCreate("live/peer")
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			result := make(chan error, 1)
			go func() {
				result <- local.pullRelay(ctx, "rtp://"+remote.Listener.Addr().String()+"/live/peer", destination)
			}()
			select {
			case <-ready:
			case err := <-result:
				t.Fatalf("peer signaling failed: %v", err)
			case <-ctx.Done():
				t.Fatal("peer signaling never activated")
			}
			reader := destination.RingBuffer().NewReaderAt(destination.StartupSnapshot().LiveCursor)
			source.WriteFrameForPublisher(pub, frame)
			received, ok, err := core.ReadFrameContext(ctx, reader)
			if err != nil || !ok || received.Codec != frame.Codec || !bytes.Equal(received.Payload, frame.Payload) {
				t.Errorf("peer media delivery: frame=%+v ok=%v error=%v", received, ok, err)
			}
			cancel()
			if err := <-result; err != nil {
				t.Errorf("pull shutdown: %v", err)
			}
		})
	}
}
