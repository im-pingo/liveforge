package sipgateway

import (
	"net"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	pionrtp "github.com/pion/rtp/v2"
)

func TestCallSessionOutboundGenerationStartupSkipsStaleHistory(t *testing.T) {
	stream := core.NewStream("sip/startup", config.StreamConfig{
		GOPCache:       true,
		GOPCacheNum:    1,
		RingBufferSize: 32,
	}, config.LimitsConfig{}, core.NewEventBus())
	defer stream.Close()

	codec := negotiatedCodec{Codec: avframe.CodecG711A, PT: 8, ClockRate: 8000, EncodingName: "PCMA"}
	videoCodec := negotiatedCodec{Codec: avframe.CodecH264, PT: 96, ClockRate: 90000, EncodingName: "H264"}
	old := &gatewayTestPublisher{id: "publisher-a", info: &avframe.MediaInfo{
		VideoCodec: avframe.CodecH264,
		AudioCodec: avframe.CodecG711A,
		SampleRate: 8000,
		Channels:   1,
	}}
	if err := stream.SetPublisher(old); err != nil {
		t.Fatal(err)
	}
	stream.WriteFrame(startupTestFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader, []byte{1, 0x64, 0, 0x1f}))
	stream.WriteFrame(startupTestFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, []byte{0x65, 0xaa}))
	stream.WriteFrame(startupTestFrame(avframe.MediaTypeAudio, avframe.CodecG711A, avframe.FrameTypeInterframe, []byte{0xa1}))

	stream.RemovePublisher()
	current := &gatewayTestPublisher{id: "publisher-b", info: old.info}
	if err := stream.SetPublisher(current); err != nil {
		t.Fatal(err)
	}
	stream.WriteFrame(startupTestFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader, []byte{1, 0x42, 0, 0x1e}))
	stream.WriteFrame(startupTestFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, []byte{0x65, 0xbb}))

	remoteRTP, remoteRTCP, err := listenLabUDPPair()
	if err != nil {
		t.Fatal(err)
	}
	defer remoteRTP.Close()
	defer remoteRTCP.Close()
	localRTP, localRTCP, err := listenLabUDPPair()
	if err != nil {
		t.Fatal(err)
	}
	localRTPPort := localRTP.LocalAddr().(*net.UDPAddr).Port
	localRTCPPort := localRTCP.LocalAddr().(*net.UDPAddr).Port
	localRTP.Close()
	localRTCP.Close()
	localVideoRTP, localVideoRTCP, err := listenLabUDPPair()
	if err != nil {
		t.Fatal(err)
	}
	localVideoRTPPort := localVideoRTP.LocalAddr().(*net.UDPAddr).Port
	localVideoRTCPPort := localVideoRTCP.LocalAddr().(*net.UDPAddr).Port
	localVideoRTP.Close()
	localVideoRTCP.Close()

	call := newCallSession("startup-call", stream.Key(), codec, "outbound", localRTPPort, localRTCPPort)
	call.configureVideo(videoCodec, localVideoRTPPort, localVideoRTCPPort, "127.0.0.1", remoteRTP.LocalAddr().(*net.UDPAddr).Port)
	defer call.Close()
	if err := call.startOutbound(stream, "127.0.0.1", remoteRTP.LocalAddr().(*net.UDPAddr).Port); err != nil {
		t.Fatal(err)
	}

	type packetResult struct {
		old      bool
		replay   int
		liveSeen bool
	}
	replayReady := make(chan struct{})
	results := make(chan packetResult, 1)
	go func() {
		result := packetResult{}
		for {
			var packet pionrtp.Packet
			buf := make([]byte, 2048)
			if err := remoteRTP.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
				results <- result
				return
			}
			n, _, err := remoteRTP.ReadFromUDP(buf)
			if err != nil {
				results <- result
				return
			}
			if packet.Unmarshal(buf[:n]) != nil {
				continue
			}
			if containsByte(packet.Payload, 0xaa) {
				result.old = true
			}
			if containsByte(packet.Payload, 0xbb) {
				result.replay++
				if result.replay == 1 {
					close(replayReady)
				}
			}
			if containsByte(packet.Payload, 0xbc) {
				result.liveSeen = true
				results <- result
				return
			}
		}
	}()

	select {
	case <-replayReady:
	case <-time.After(2 * time.Second):
		t.Fatal("SIP call did not send publisher-B replay keyframe")
	}
	stream.WriteFrame(startupTestFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe, []byte{0x41, 0xbc}))
	var result packetResult
	select {
	case result = <-results:
	case <-time.After(2 * time.Second):
		t.Fatal("SIP call did not send publisher-B live frame")
	}
	if result.old {
		t.Fatal("SIP call sent publisher-A media from retained ring history")
	}
	if result.replay != 1 || !result.liveSeen {
		t.Fatalf("publisher-B replay/live packets = %+v, want one replay and one live frame", result)
	}
}

func startupTestFrame(mediaType avframe.MediaType, codec avframe.CodecType, frameType avframe.FrameType, payload []byte) *avframe.AVFrame {
	return avframe.NewAVFrame(mediaType, codec, frameType, 0, 0, payload)
}

func containsByte(payload []byte, want byte) bool {
	for _, value := range payload {
		if value == want {
			return true
		}
	}
	return false
}
