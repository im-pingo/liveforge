package cluster

import (
	"net"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/muxer/ps"
	pionrtp "github.com/pion/rtp/v2"
)

func TestGBTransportScheme(t *testing.T) {
	// Verify interface compliance without starting listeners.
	var _ RelayTransport = (*GBTransport)(nil)
}

func TestGBTransportSendPSFrame(t *testing.T) {
	tr := &GBTransport{}
	if tr.Scheme() != "gb28181" {
		t.Errorf("Scheme() = %q, want gb28181", tr.Scheme())
	}
}

func TestGBTransportPSRoundTrip(t *testing.T) {
	// Open a local UDP listener to receive PS-over-RTP packets.
	recv, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer recv.Close()
	recvPort := recv.LocalAddr().(*net.UDPAddr).Port

	// Open a UDP sender.
	sender, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: recvPort})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer sender.Close()

	// Build a real PS pack from a H.264 keyframe.
	muxer := ps.NewMuxer()
	startCode := []byte{0x00, 0x00, 0x00, 0x01}
	sps := []byte{0x67, 0x42, 0x00, 0x1E, 0xAB, 0x40, 0x50}
	pps := []byte{0x68, 0xCE, 0x38, 0x80}
	idr := make([]byte, 100)
	idr[0] = 0x65
	for i := 1; i < len(idr); i++ {
		idr[i] = byte(i)
	}

	var payload []byte
	payload = append(payload, startCode...)
	payload = append(payload, sps...)
	payload = append(payload, startCode...)
	payload = append(payload, pps...)
	payload = append(payload, startCode...)
	payload = append(payload, idr...)

	frame := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 1000, 1000, payload)
	psData, err := muxer.Pack(frame)
	if err != nil {
		t.Fatalf("muxer.Pack: %v", err)
	}

	// Use sendPSFrame to send PS data as fragmented RTP.
	tr := &GBTransport{}
	var seq uint16
	var ts uint32
	ssrc := uint32(12345)

	if err := tr.sendPSFrame(sender, muxer, frame, &seq, &ts, ssrc); err != nil {
		t.Fatalf("sendPSFrame: %v", err)
	}

	// Read all RTP packets from receiver and reassemble PS data.
	var reassembled []byte
	buf := make([]byte, 2048)
	recv.SetReadDeadline(time.Now().Add(2 * time.Second))

	for {
		n, _, err := recv.ReadFromUDP(buf)
		if err != nil {
			break
		}
		var pkt pionrtp.Packet
		if err := pkt.Unmarshal(buf[:n]); err != nil {
			t.Fatalf("unmarshal RTP: %v", err)
		}
		reassembled = append(reassembled, pkt.Payload...)

		if pkt.Marker {
			break // last fragment
		}
	}

	if len(reassembled) == 0 {
		t.Fatal("no PS data received")
	}

	// Demux the reassembled PS data and verify we get a video frame.
	demuxer := ps.NewDemuxer()
	frames, err := demuxer.Feed(reassembled)
	if err != nil {
		t.Fatalf("demuxer.Feed: %v", err)
	}

	if len(frames) == 0 {
		t.Fatal("no frames after demux")
	}

	hasVideo := false
	for _, f := range frames {
		if f.MediaType == avframe.MediaTypeVideo {
			hasVideo = true
		}
	}
	if !hasVideo {
		t.Error("no video frame in demuxed output")
	}

	// Verify PS data sizes match.
	if len(reassembled) != len(psData) {
		t.Errorf("reassembled size = %d, original PS size = %d", len(reassembled), len(psData))
	}
}

func TestGBTransportClose(t *testing.T) {
	tr := &GBTransport{}
	if err := tr.Close(); err != nil {
		t.Errorf("Close() = %v, want nil", err)
	}
}
