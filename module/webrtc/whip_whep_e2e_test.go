package webrtc

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	pkgRTP "github.com/im-pingo/liveforge/pkg/rtp"
	pionrtp "github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// TestWHIPToWHEPStartsAfterIngestedKeyframe covers the same handoff used by
// the Console: a WHIP publisher sends H.264, then a WHEP viewer joins the
// already-publishing stream. The viewer must receive the cached IDR instead
// of remaining in waiting_keyframe indefinitely.
func TestWHIPToWHEPStartsAfterIngestedKeyframe(t *testing.T) {
	m, server := newTestModule(t)
	const streamKey = "live/whip-whep-keyframe"

	whipPC, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = whipPC.Close() })
	whipTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000},
		"video", "whip-whep-source",
	)
	if err != nil {
		t.Fatal(err)
	}
	whipSender, err := whipPC.AddTrack(whipTrack)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			if _, _, readErr := whipSender.ReadRTCP(); readErr != nil {
				return
			}
		}
	}()

	whipOffer, err := whipPC.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	whipGathered := webrtc.GatheringCompletePromise(whipPC)
	if err := whipPC.SetLocalDescription(whipOffer); err != nil {
		t.Fatal(err)
	}
	<-whipGathered
	whipReq := httptest.NewRequest(http.MethodPost, "/webrtc/whip/"+streamKey, bytes.NewBufferString(whipPC.LocalDescription().SDP))
	whipReq.Header.Set("Content-Type", "application/sdp")
	whipResp := httptest.NewRecorder()
	m.httpSrv.Handler.ServeHTTP(whipResp, whipReq)
	if whipResp.Code != http.StatusCreated {
		t.Fatalf("WHIP status = %d, want 201: %s", whipResp.Code, whipResp.Body.String())
	}
	if err := whipPC.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: whipResp.Body.String()}); err != nil {
		t.Fatal(err)
	}
	waitPeerConnected(t, whipPC)
	time.Sleep(100 * time.Millisecond)

	seqHeader, frames := loadH264TestFixture(t, "testdata/test_320x180.h264")
	keyframeIndex := -1
	for index, frame := range frames {
		if frame.isKeyframe {
			keyframeIndex = index
			break
		}
	}
	if keyframeIndex < 0 {
		t.Fatal("H.264 fixture has no keyframe")
	}
	t.Logf("first fixture keyframe index=%d bytes=%d", keyframeIndex, len(frames[keyframeIndex].avccPayload))
	packetizer := &pkgRTP.H264Packetizer{}
	sequence := uint16(1)
	writeFrame := func(frame *avframe.AVFrame) {
		t.Helper()
		packets, packetErr := packetizer.Packetize(frame, 1200)
		if packetErr != nil {
			t.Fatal(packetErr)
		}
		t.Logf("sending WHIP frame type=%v bytes=%d packets=%d", frame.FrameType, len(frame.Payload), len(packets))
		for _, packet := range packets {
			if err := whipTrack.WriteRTP(&pionrtp.Packet{
				Header:  pionrtp.Header{Version: 2, Marker: packet.Marker, SequenceNumber: sequence, Timestamp: uint32(sequence * 3000)},
				Payload: append([]byte(nil), packet.Payload...),
			}); err != nil {
				t.Fatal(err)
			}
			sequence++
		}
	}
	writeFrame(avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader, 0, 0, seqHeader))
	writeFrame(avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 40, 40, frames[keyframeIndex].avccPayload))

	var stream *core.Stream
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		candidate, ok := server.StreamHub().Find(streamKey)
		if ok {
			startup := candidate.StartupSnapshot()
			if startup.Ready && startup.MediaInfo.VideoCodec == avframe.CodecH264 {
				stream = candidate
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if stream == nil {
		candidate, _ := server.StreamHub().Find(streamKey)
		if candidate != nil {
			startup := candidate.StartupSnapshot()
			t.Fatalf("WHIP stream did not become ready after SPS/PPS+IDR: cursor=%d ready=%v media=%+v", candidate.RingBuffer().WriteCursor(), startup.Ready, startup.MediaInfo)
		}
		t.Fatal("WHIP stream did not become ready after SPS/PPS+IDR")
	}

	whepPC, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = whepPC.Close() })
	packets := make(chan struct{}, 1)
	tracks := make(chan struct{}, 1)
	whepPC.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		select {
		case tracks <- struct{}{}:
		default:
		}
		go func() {
			for {
				if _, _, readErr := track.ReadRTP(); readErr != nil {
					return
				}
				select {
				case packets <- struct{}{}:
				default:
				}
			}
		}()
	})
	if _, err := whepPC.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		t.Fatal(err)
	}
	whepOffer, err := whepPC.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	whepGathered := webrtc.GatheringCompletePromise(whepPC)
	if err := whepPC.SetLocalDescription(whepOffer); err != nil {
		t.Fatal(err)
	}
	<-whepGathered
	whepReq := httptest.NewRequest(http.MethodPost, "/webrtc/whep/"+streamKey+"?mode=live", bytes.NewBufferString(whepPC.LocalDescription().SDP))
	whepReq.Header.Set("Content-Type", "application/sdp")
	whepResp := httptest.NewRecorder()
	m.httpSrv.Handler.ServeHTTP(whepResp, whepReq)
	if whepResp.Code != http.StatusCreated {
		t.Fatalf("WHEP status = %d, want 201: %s", whepResp.Code, whepResp.Body.String())
	}
	if err := whepPC.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: whepResp.Body.String()}); err != nil {
		t.Fatal(err)
	}
	waitPeerConnected(t, whepPC)

	select {
	case <-packets:
	case <-time.After(3 * time.Second):
		select {
		case <-tracks:
		default:
			statusReq := httptest.NewRequest(http.MethodGet, whepResp.Header().Get("Location")+"/status", nil)
			statusResp := httptest.NewRecorder()
			m.httpSrv.Handler.ServeHTTP(statusResp, statusReq)
			t.Fatalf("WHEP viewer did not receive RTP/ontrack; status=%s answer=%s", statusResp.Body.String(), whepResp.Body.String())
		}
	}

	location := whepResp.Header().Get("Location")
	if location == "" {
		t.Fatal("WHEP response did not include a session Location")
	}
	statusReq := httptest.NewRequest(http.MethodGet, location+"/status", nil)
	statusResp := httptest.NewRecorder()
	m.httpSrv.Handler.ServeHTTP(statusResp, statusReq)
	if statusResp.Code != http.StatusOK {
		t.Fatalf("WHEP status endpoint = %d: %s", statusResp.Code, statusResp.Body.String())
	}
	var status struct {
		Feed WHEPFeedStatus `json:"feed"`
	}
	if err := json.Unmarshal(statusResp.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Feed.State != WHEPFeedPlaying || status.Feed.VideoFrames == 0 || status.Feed.RTPPacketsSent == 0 {
		t.Fatalf("WHEP feed status = %+v, want playing with sent video", status.Feed)
	}
}

func waitPeerConnected(t *testing.T, pc *webrtc.PeerConnection) {
	t.Helper()
	if pc.ConnectionState() == webrtc.PeerConnectionStateConnected {
		return
	}
	connected := make(chan struct{})
	var once sync.Once
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateConnected {
			once.Do(func() { close(connected) })
		}
	})
	select {
	case <-connected:
	case <-time.After(5 * time.Second):
		t.Fatalf("PeerConnection state = %s, want connected", pc.ConnectionState())
	}
}
