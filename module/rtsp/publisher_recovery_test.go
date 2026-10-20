package rtsp

import (
	"bytes"
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/pkg/avframe"
	pkgrtp "github.com/im-pingo/liveforge/pkg/rtp"
	pionrtp "github.com/pion/rtp/v2"
)

func rtspVideoPacket(seq uint16, ts uint32, marker bool, payload ...byte) *pionrtp.Packet {
	return &pionrtp.Packet{Header: pionrtp.Header{Version: 2, PayloadType: 96, SequenceNumber: seq, Timestamp: ts, SSRC: 99, Marker: marker}, Payload: payload}
}

func TestRTSPIngressRecoversAfterPacketLoss(t *testing.T) {
	for _, protocol := range []string{"UDP", "TCP"} {
		t.Run(protocol, func(t *testing.T) {
			stream, publisher, session := newRTSPIngressSession(t, "recovery-"+protocol)
			defer publisher.Close()
			reader := stream.RingBuffer().NewReader()
			var feed func(*pionrtp.Packet)
			if protocol == "UDP" {
				transport := newEphemeralRTSPUDPTransport(t)
				done := make(chan struct{})
				go func() { new(Module).udpPublishLoop(transport, session); close(done) }()
				defer func() { transport.Close(); <-done }()
				port, _ := transport.ServerPorts()
				client, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: port})
				if err != nil {
					t.Fatal(err)
				}
				defer client.Close()
				feed = func(pkt *pionrtp.Packet) {
					data, err := pkt.Marshal()
					if err != nil {
						t.Fatal(err)
					}
					if _, err := client.Write(data); err != nil {
						t.Fatal(err)
					}
				}
			} else {
				feed = func(pkt *pionrtp.Packet) {
					data, err := pkt.Marshal()
					if err != nil {
						t.Fatal(err)
					}
					if err := new(Module).processInterleaved(session, 0, data); err != nil {
						t.Fatalf("recoverable media packet closed TCP ingress: %v", err)
					}
				}
			}
			feed(rtspVideoPacket(9, 100, false, 0x61, 0xaa))
			feed(rtspVideoPacket(10, 100, false, 0x7c, 0x85, 0xbb))
			// Sequence 11 is lost. Neither the accumulated NAL nor this FU may escape.
			feed(rtspVideoPacket(12, 100, true, 0x7c, 0x45, 0xcc))
			feed(rtspVideoPacket(13, 200, true, 0x65, 0xdd))
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			frame, ok := reader.ReadContext(ctx)
			if !ok {
				t.Fatal("receiver failed to recover on the next complete access unit")
			}
			if !bytes.Equal(frame.Payload, []byte{0, 0, 0, 2, 0x65, 0xdd}) {
				t.Fatalf("recovered payload includes damaged data: %x", frame.Payload)
			}
			if stream.Publisher() != publisher || session.Snapshot().Closed {
				t.Fatal("media loss ended the publishing session")
			}
		})
	}
}

func TestRTSPPublisherRejectsOversizedAccessUnitAndRecovers(t *testing.T) {
	stream, publisher, _ := newRTSPIngressSession(t, "bounded-au")
	defer publisher.Close()
	reader := stream.RingBuffer().NewReader()
	payload := make([]byte, 1400)
	payload[0] = 0x65
	for i := 0; i < 14000; i++ {
		if err := publisher.FeedRTP(rtspVideoPacket(uint16(i), 100, i == 13999, payload...)); err != nil {
			t.Fatal(err)
		}
	}
	if frame, ok := reader.TryRead(); ok {
		t.Fatalf("oversized access unit emitted: %d bytes", len(frame.Payload))
	}
	if err := publisher.FeedRTP(rtspVideoPacket(14000, 200, true, 0x65, 0xdd)); err != nil {
		t.Fatal(err)
	}
	frame, ok := reader.TryRead()
	if !ok || !bytes.Equal(frame.Payload, []byte{0, 0, 0, 2, 0x65, 0xdd}) {
		t.Fatalf("fresh unit did not recover cleanly: %v", frame)
	}
}

func TestRTSPPublisherKeepsTrackAndTimestampUnitsSeparate(t *testing.T) {
	stream, publisher, _ := newRTSPIngressSession(t, "separate-units")
	defer publisher.Close()
	publisher.depacketizers[97] = &pkgrtp.H264Depacketizer{}
	publisher.codecs[97] = avframe.CodecH264
	reader := stream.RingBuffer().NewReader()
	if err := publisher.FeedRTP(rtspVideoPacket(1, 100, false, 0x65, 0xaa)); err != nil {
		t.Fatal(err)
	}
	other := rtspVideoPacket(10, 200, true, 0x65, 0xbb)
	other.PayloadType = 97
	if err := publisher.FeedRTP(other); err != nil {
		t.Fatal(err)
	}
	// A later timestamp abandons the unmarked first unit on track 96.
	if err := publisher.FeedRTP(rtspVideoPacket(2, 300, true, 0x65, 0xcc)); err != nil {
		t.Fatal(err)
	}
	for _, want := range [][]byte{{0, 0, 0, 2, 0x65, 0xbb}, {0, 0, 0, 2, 0x65, 0xcc}} {
		frame, ok := reader.TryRead()
		if !ok || !bytes.Equal(frame.Payload, want) {
			t.Fatalf("mixed access units: frame=%v want=%x", frame, want)
		}
	}
}

func TestRTSPPublisherConcurrentTracks(t *testing.T) {
	stream, publisher, _ := newRTSPIngressSession(t, "concurrent-tracks")
	defer publisher.Close()
	publisher.depacketizers[97] = &pkgrtp.H264Depacketizer{}
	publisher.codecs[97] = avframe.CodecH264
	var wg sync.WaitGroup
	for _, pt := range []uint8{96, 97} {
		wg.Go(func() {
			for i := 0; i < 100; i++ {
				pkt := rtspVideoPacket(uint16(i), uint32(i*3000), true, 0x65, pt)
				pkt.PayloadType = pt
				if err := publisher.FeedRTP(pkt); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	wg.Wait()
	if got := stream.RingBuffer().WriteCursor(); got != 200 {
		t.Fatalf("concurrent frames=%d want=200", got)
	}
	reader := stream.RingBuffer().NewReaderAt(184)
	for i := 0; i < 16; i++ {
		frame, ok := reader.TryRead()
		if !ok || frame.Codec != avframe.CodecH264 || len(frame.Payload) != 6 {
			t.Fatalf("corrupted concurrent output: %v", frame)
		}
	}
}

func TestRTSPPublisherPreservesSeparateSequenceHeader(t *testing.T) {
	stream, publisher, _ := newRTSPIngressSession(t, "sequence-header")
	defer publisher.Close()
	reader := stream.RingBuffer().NewReader()
	for i, payload := range [][]byte{{0x67, 0x42, 0, 0x1f}, {0x68, 0xce, 0x06}, {0x65, 0xaa}} {
		if err := publisher.FeedRTP(rtspVideoPacket(uint16(i), 100, i == 2, payload...)); err != nil {
			t.Fatal(err)
		}
	}
	header, ok := reader.TryRead()
	if !ok || header.FrameType != avframe.FrameTypeSequenceHeader || header.Payload[0] != 1 {
		t.Fatalf("missing separate AVC configuration: %v", header)
	}
	frame, ok := reader.TryRead()
	if !ok || frame.FrameType != avframe.FrameTypeKeyframe || !bytes.Equal(frame.Payload, []byte{0, 0, 0, 2, 0x65, 0xaa}) {
		t.Fatalf("configuration merged into media: %v", frame)
	}
	if cached := stream.VideoSeqHeader(); cached != header {
		t.Fatal("sequence header missing from startup cache")
	}
}
