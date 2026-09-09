package webrtc

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/muxer/fmp4"
	pkgrtp "github.com/im-pingo/liveforge/pkg/rtp"
	"github.com/pion/rtp"
	rtpv2 "github.com/pion/rtp/v2"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
)

func newSimulcastPeer(t *testing.T, m *Module, mime, key string, extraTracks ...*webrtc.TrackLocalStaticRTP) (*webrtc.PeerConnection, []*webrtc.TrackLocalStaticRTP, func(int, []byte, bool)) {
	t.Helper()
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	tracks := make([]*webrtc.TrackLocalStaticRTP, 0, 3)
	var sender *webrtc.RTPSender
	for _, rid := range []string{"l", "m", "h"} {
		track, trackErr := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: mime, ClockRate: 90000}, "video", "simulcast", webrtc.WithRTPStreamID(rid))
		if trackErr != nil {
			t.Fatal(trackErr)
		}
		if sender == nil {
			sender, err = pc.AddTrack(track)
		} else {
			err = sender.AddEncoding(track)
		}
		if err != nil {
			t.Fatal(err)
		}
		tracks = append(tracks, track)
	}
	for _, track := range extraTracks {
		if _, addErr := pc.AddTrack(track); addErr != nil {
			t.Fatal(addErr)
		}
	}
	go func() {
		for {
			if _, _, readErr := sender.ReadRTCP(); readErr != nil {
				return
			}
		}
	}()
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	<-gathered
	req := httptest.NewRequest(http.MethodPost, "/webrtc/whip/"+key, bytes.NewBufferString(pc.LocalDescription().SDP))
	req.Header.Set("Content-Type", "application/sdp")
	res := httptest.NewRecorder()
	m.httpSrv.Handler.ServeHTTP(res, req)
	if res.Code != http.StatusCreated {
		t.Fatalf("WHIP: %d %s", res.Code, res.Body.String())
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: res.Body.String()}); err != nil {
		t.Fatal(err)
	}
	waitPeerConnected(t, pc)
	var midID, ridID uint8
	for _, ext := range sender.GetParameters().HeaderExtensions {
		if ext.ID <= 0 || ext.ID > 255 {
			t.Fatalf("invalid RTP extension ID %d", ext.ID)
			continue
		}
		if ext.URI == sdp.SDESMidURI {
			midID = uint8(ext.ID)
		}
		if ext.URI == sdp.SDESRTPStreamIDURI {
			ridID = uint8(ext.ID)
		}
	}
	if midID == 0 || ridID == 0 {
		t.Fatal("RID extensions not negotiated")
	}
	var seq [3]uint16
	write := func(layer int, payload []byte, marker bool) {
		t.Helper()
		seq[layer]++
		packet := &rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: seq[layer], Timestamp: uint32(seq[layer]) * 3000, Marker: marker}, Payload: payload}
		if err := packet.SetExtension(midID, []byte("0")); err != nil {
			t.Fatal(err)
		}
		if err := packet.SetExtension(ridID, []byte(tracks[layer].RID())); err != nil {
			t.Fatal(err)
		}
		if err := tracks[layer].WriteRTP(packet); err != nil {
			t.Fatal(err)
		}
	}
	return pc, tracks, write
}

func TestWHIPSimulcastSharedAudioIsDeclaredBeforePublish(t *testing.T) {
	m, server := newTestModule(t)
	cfg := *server.Config()
	cfg.Stream.Simulcast = simulcastTestConfig(false)
	server.UpdateConfig(&cfg)
	audio, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, "audio", "simulcast")
	if err != nil {
		t.Fatal(err)
	}
	_, _, write := newSimulcastPeer(t, m, webrtc.MimeTypeVP8, "live/audio-layers", audio)
	write(0, []byte{0x10, 0, 1}, true)
	time.Sleep(50 * time.Millisecond)
	parent, _ := server.StreamHub().Find("live/audio-layers")
	if parent.State() == core.StreamStatePublishing {
		t.Error("publisher lifecycle started before advertised audio codec was known")
	}
	for i := uint16(1); i <= 3; i++ {
		if writeErr := audio.WriteRTP(&rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: i, Timestamp: uint32(i) * 960}, Payload: []byte{0xf8, 0xff, 0xfe}}); writeErr != nil {
			t.Fatal(writeErr)
		}
	}
	family := awaitSimulcast(t, server, "live/audio-layers", func(f *core.SimulcastFamily) bool {
		for _, rid := range []string{"h", "m", "l"} {
			layer, _ := f.Layer(rid)
			if layer.Stats().AudioFrames != 3 {
				return false
			}
		}
		return true
	})
	for _, rid := range []string{"h", "m", "l"} {
		layer, _ := family.Layer(rid)
		info := layer.StartupSnapshot().MediaInfo
		if info.AudioCodec != avframe.CodecOpus || info.SampleRate != 48000 || info.Channels != 2 {
			t.Fatalf("%s shared audio metadata %+v", rid, info)
		}
	}
}

func TestWHIPSimulcastReplacementJoinsIngestAndRejectsLateTracks(t *testing.T) {
	m, server := newTestModule(t)
	cfg := *server.Config()
	cfg.Stream.Simulcast = simulcastTestConfig(false)
	server.UpdateConfig(&cfg)
	pc, _, write := newSimulcastPeer(t, m, webrtc.MimeTypeVP8, "live/replace-layers")
	write(0, []byte{0x10, 0, 1}, true)
	family := awaitSimulcast(t, server, "live/replace-layers", func(*core.SimulcastFamily) bool { return true })
	parent, _ := server.StreamHub().Find("live/replace-layers")
	old := parent.Publisher()
	sess, ok := m.findSession(old.ID())
	if !ok {
		t.Fatal("WHIP session missing")
	}
	parent.RemovePublisherIf(old)
	replacement := &authorizationTestPublisher{id: "new-generation", info: &avframe.MediaInfo{VideoCodec: avframe.CodecVP8}}
	if setErr := parent.SetPublisher(replacement); setErr != nil {
		t.Fatal(setErr)
	}
	start := parent.StartupSnapshot().LiveCursor
	for i := range 3 {
		write(i, []byte{0x10, 0, byte(i)}, true)
	}
	joined := make(chan struct{})
	go func() { sess.Close(); close(joined) }()
	select {
	case <-joined:
	case <-time.After(3 * time.Second):
		t.Fatal("WHIP close did not join ingest loops")
	}
	if parent.Publisher() != replacement || parent.RingBuffer().WriteCursor() != start {
		t.Fatal("late OnTrack changed replacement")
	}
	for _, rid := range []string{"l", "m"} {
		layer, _ := family.Layer(rid)
		if layer.State() != core.StreamStateDestroying {
			t.Fatal("private layer retained after replacement")
		}
	}
	if closeErr := pc.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
}

func simulcastH264Fixtures(t *testing.T) ([3][]byte, [3][]byte) {
	t.Helper()
	var headers, keys [3][]byte
	for i, path := range []string{"testdata/simulcast_80x46.h264", "testdata/simulcast_160x90.h264", "testdata/test_320x180.h264"} {
		header, frames := loadH264TestFixture(t, path)
		headers[i] = header
		for _, frame := range frames {
			if frame.isKeyframe {
				keys[i] = frame.avccPayload
				break
			}
		}
		if len(keys[i]) == 0 {
			t.Fatal("fixture has no IDR")
		}
	}
	return headers, keys
}

func sendSimulcastH264(t *testing.T, write func(int, []byte, bool), index int, header, key []byte) {
	t.Helper()
	packetizer := &pkgrtp.H264Packetizer{}
	for _, frame := range []*avframe.AVFrame{
		avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader, 0, 0, header),
		avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 40, 40, key),
	} {
		if len(frame.Payload) == 0 {
			continue
		}
		packets, err := packetizer.Packetize(frame, 1200)
		if err != nil {
			t.Fatal(err)
		}
		for _, packet := range packets {
			write(index, packet.Payload, packet.Marker)
		}
	}
}

func newSimulcastWHEPViewer(t *testing.T, m *Module, key, selection string) (*webrtc.PeerConnection, <-chan *avframe.AVFrame, string) {
	t.Helper()
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	frames := make(chan *avframe.AVFrame, 64)
	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		dp := &pkgrtp.H264Depacketizer{}
		for {
			packet, _, readErr := track.ReadRTP()
			if readErr != nil {
				return
			}
			raw, marshalErr := packet.Marshal()
			if marshalErr != nil {
				return
			}
			var current rtpv2.Packet
			if current.Unmarshal(raw) != nil {
				continue
			}
			decoded, decodeErr := dp.DepacketizeFrames(&current)
			if decodeErr != nil {
				continue
			}
			for _, frame := range decoded {
				select {
				case frames <- frame:
				default:
				}
			}
		}
	})
	if _, transceiverErr := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); transceiverErr != nil {
		t.Fatal(transceiverErr)
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	<-gathered
	req := httptest.NewRequest(http.MethodPost, "/webrtc/whep/"+key+"?layer="+selection, strings.NewReader(pc.LocalDescription().SDP))
	req.Header.Set("Content-Type", "application/sdp")
	res := httptest.NewRecorder()
	m.httpSrv.Handler.ServeHTTP(res, req)
	if res.Code != http.StatusCreated {
		t.Fatalf("WHEP %s: %d %s", selection, res.Code, res.Body.String())
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: res.Body.String()}); err != nil {
		t.Fatal(err)
	}
	waitPeerConnected(t, pc)
	return pc, frames, res.Header().Get("Location")
}

func TestWHIPThreeRIDH264WHEPAndCanonicalContainer(t *testing.T) {
	m, server := newTestModule(t)
	cfg := *server.Config()
	cfg.Stream.Simulcast = simulcastTestConfig(false)
	server.UpdateConfig(&cfg)
	_, _, write := newSimulcastPeer(t, m, webrtc.MimeTypeH264, "live/h264-layers")
	headers, keys := simulcastH264Fixtures(t)
	for range 3 {
		for i := range 3 {
			sendSimulcastH264(t, write, i, headers[i], keys[i])
		}
		time.Sleep(30 * time.Millisecond)
	}
	family := awaitSimulcast(t, server, "live/h264-layers", func(f *core.SimulcastFamily) bool {
		for _, rid := range []string{"l", "m", "h"} {
			layer, _ := f.Layer(rid)
			snap := layer.StartupSnapshot()
			if !snap.Ready || len(snap.ReplayFrames) == 0 {
				return false
			}
		}
		return true
	})
	for i, rid := range []string{"l", "m", "h"} {
		layer, _ := family.Layer(rid)
		snap := layer.StartupSnapshot()
		if !bytes.Equal(snap.VideoSequenceHeader.Payload, headers[i]) {
			t.Fatalf("%s sequence header crossed RID", rid)
		}
		for _, frame := range snap.ReplayFrames {
			if frame.FrameType != avframe.FrameTypeSequenceHeader && !bytes.Equal(frame.Payload, keys[i]) {
				t.Fatalf("%s media crossed RID", rid)
			}
		}
	}
	for _, tc := range []struct {
		selector, rid string
		index         int
	}{{"low", "l", 0}, {"m", "m", 1}, {"high", "h", 2}, {"auto", "h", 2}, {"", "h", 2}} {
		pc, received, location := newSimulcastWHEPViewer(t, m, "live/h264-layers", tc.selector)
		for range 2 {
			sendSimulcastH264(t, write, tc.index, headers[tc.index], keys[tc.index])
			time.Sleep(40 * time.Millisecond)
		}
		gotHeader, gotMedia := false, 0
		deadline := time.After(4 * time.Second)
		for !gotHeader || gotMedia < 2 {
			select {
			case frame := <-received:
				if frame.FrameType == avframe.FrameTypeSequenceHeader {
					if !bytes.Equal(frame.Payload, headers[tc.index]) {
						t.Fatal("WHEP selected wrong resolution")
					}
					gotHeader = true
				} else {
					if !bytes.Equal(frame.Payload, keys[tc.index]) {
						t.Fatal("WHEP selected wrong RID media")
					}
					gotMedia++
				}
			case <-deadline:
				t.Fatalf("%s did not advance selected H264: header=%v media=%d", tc.selector, gotHeader, gotMedia)
			}
		}
		// Loopback RTP can arrive before WriteSample returns and RecordVideo
		// counts the completed send. Require the counter to catch up boundedly.
		statusDeadline := time.Now().Add(2 * time.Second)
		for {
			res := httptest.NewRecorder()
			m.httpSrv.Handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, location+"/status", nil))
			if res.Code != http.StatusOK {
				t.Fatalf("selected status HTTP %d: %s", res.Code, res.Body.String())
			}
			var status sessionStatusResponse
			if err := json.Unmarshal(res.Body.Bytes(), &status); err != nil {
				t.Fatal(err)
			}
			if status.Layer != tc.rid {
				t.Fatalf("selected status %+v", status)
			}
			if status.Feed.VideoFrames >= 2 {
				break
			}
			if time.Now().After(statusDeadline) {
				t.Fatalf("selected status did not count received frames: %+v", status)
			}
			time.Sleep(5 * time.Millisecond)
		}
		pc.Close()
		m.httpSrv.Handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodDelete, location, nil))
	}
	parent, _ := server.StreamHub().Find("live/h264-layers")
	snap := parent.StartupSnapshot()
	mux := fmp4.NewMuxer(avframe.CodecH264, 0)
	init := mux.Init(snap.VideoSequenceHeader, nil, 0, 0, 0, 0)
	media := mux.WriteSegment(snap.ReplayFrames)
	demux, err := fmp4.NewDemuxer(init)
	if err != nil {
		t.Fatal(err)
	}
	frames, err := demux.Parse(media)
	if err != nil || len(frames) == 0 {
		t.Fatalf("canonical fMP4: %v", err)
	}
	for _, frame := range frames {
		if !bytes.Equal(frame.Payload, keys[2]) {
			t.Fatal("canonical container includes noncanonical media")
		}
	}
	if binary, err := exec.LookPath("ffmpeg"); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, "-hide_banner", "-loglevel", "error", "-i", "pipe:0", "-f", "null", "-")
		cmd.Stdin = bytes.NewReader(append(init, media...))
		if output, err := cmd.CombinedOutput(); err != nil || len(output) > 0 {
			t.Fatalf("canonical container did not decode: %v %s", err, output)
		}
	} else {
		t.Log("FFmpeg executable unavailable; container roundtrip checked without external decoding")
	}
}

func TestWHIPSimulcastH264PauseResumeRequiresFreshHeaders(t *testing.T) {
	m, server := newTestModule(t)
	cfg := *server.Config()
	cfg.Stream.Simulcast = simulcastTestConfig(true)
	server.UpdateConfig(&cfg)
	_, _, write := newSimulcastPeer(t, m, webrtc.MimeTypeH264, "live/h264-pause")
	headers, keys := simulcastH264Fixtures(t)
	for range 3 {
		for i := range 3 {
			sendSimulcastH264(t, write, i, headers[i], keys[i])
		}
		time.Sleep(20 * time.Millisecond)
	}
	family := awaitSimulcast(t, server, "live/h264-pause", func(f *core.SimulcastFamily) bool {
		high, _ := f.Layer("h")
		return len(high.StartupSnapshot().ReplayFrames) > 0
	})
	low, _ := family.Layer("l")
	if low.RingBuffer().WriteCursor() != 0 {
		t.Fatal("unused low layer was depacketized")
	}
	_, _, release, err := family.Acquire("low", "webrtc")
	if err != nil {
		t.Fatal(err)
	}
	sendSimulcastH264(t, write, 0, headers[0], keys[0])
	awaitSimulcast(t, server, "live/h264-pause", func(*core.SimulcastFamily) bool { return len(low.StartupSnapshot().ReplayFrames) > 0 })
	release()
	_, _, release, err = family.Acquire("low", "webrtc")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	before := low.RingBuffer().WriteCursor()
	sendSimulcastH264(t, write, 0, nil, keys[0])
	time.Sleep(100 * time.Millisecond)
	if low.RingBuffer().WriteCursor() != before || low.StartupSnapshot().VideoSequenceHeader != nil {
		t.Fatal("resume reused old depacketizer headers")
	}
	sendSimulcastH264(t, write, 0, headers[1], keys[1])
	awaitSimulcast(t, server, "live/h264-pause", func(*core.SimulcastFamily) bool { return len(low.StartupSnapshot().ReplayFrames) > 0 })
	if !bytes.Equal(low.StartupSnapshot().VideoSequenceHeader.Payload, headers[1]) {
		t.Fatal("resume failed to adopt fresh headers")
	}
}

func awaitSimulcast(t *testing.T, server *core.Server, key string, ready func(*core.SimulcastFamily) bool) *core.SimulcastFamily {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if stream, ok := server.StreamHub().Find(key); ok {
			if family := stream.Simulcast(); family != nil && ready(family) {
				return family
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("simulcast family not ready")
	return nil
}

func TestWHIPThreeRIDIsolatedVP8(t *testing.T) {
	m, server := newTestModule(t)
	cfg := *server.Config()
	cfg.Stream.Simulcast = simulcastTestConfig(false)
	server.UpdateConfig(&cfg)
	_, tracks, write := newSimulcastPeer(t, m, webrtc.MimeTypeVP8, "live/three-rid")
	for range 5 {
		for i := range tracks {
			write(i, []byte{0x90, 0x80, 0x81, byte(i), 0, byte(i + 1)}, true)
		}
		time.Sleep(20 * time.Millisecond)
	}
	family := awaitSimulcast(t, server, "live/three-rid", func(f *core.SimulcastFamily) bool {
		for _, rid := range []string{"l", "m", "h"} {
			s, _ := f.Layer(rid)
			if s.Stats().VideoFrames < 1 {
				return false
			}
		}
		return true
	})
	for i, track := range tracks {
		stream, _ := family.Layer(track.RID())
		for _, frame := range stream.StartupSnapshot().ReplayFrames {
			if len(frame.Payload) != 2 || frame.Payload[1] != byte(i+1) {
				t.Fatalf("RID %s mixed frame %v", track.RID(), frame.Payload)
			}
		}
	}
}

func TestWHIPOrdinaryVP8ExtendedDescriptor(t *testing.T) {
	m, server := newTestModule(t)
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	track, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000}, "video", "ordinary-vp8")
	if err != nil {
		t.Fatal(err)
	}
	if _, addErr := pc.AddTrack(track); addErr != nil {
		t.Fatal(addErr)
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if localErr := pc.SetLocalDescription(offer); localErr != nil {
		t.Fatal(localErr)
	}
	<-gathered
	req := httptest.NewRequest(http.MethodPost, "/webrtc/whip/live/ordinary-vp8", strings.NewReader(pc.LocalDescription().SDP))
	req.Header.Set("Content-Type", "application/sdp")
	res := httptest.NewRecorder()
	m.httpSrv.Handler.ServeHTTP(res, req)
	if res.Code != http.StatusCreated {
		t.Fatalf("WHIP status=%d %s", res.Code, res.Body.String())
	}
	if remoteErr := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: res.Body.String()}); remoteErr != nil {
		t.Fatal(remoteErr)
	}
	waitPeerConnected(t, pc)
	if writeErr := track.WriteRTP(&rtp.Packet{Header: rtp.Header{Version: 2, Marker: true, SequenceNumber: 1, Timestamp: 3000}, Payload: []byte{0x90, 0xf0, 0x81, 0x23, 2, 0xa1, 0, 0x55}}); writeErr != nil {
		t.Fatal(writeErr)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		stream, _ := server.StreamHub().Find("live/ordinary-vp8")
		if replay := stream.StartupSnapshot().ReplayFrames; len(replay) > 0 {
			if !bytes.Equal(replay[0].Payload, []byte{0, 0x55}) {
				t.Fatal("ordinary WHIP leaked VP8 descriptor into media")
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("ordinary WHIP VP8 did not arrive")
}

func TestWHIPDisabledSimulcastCreatesNoState(t *testing.T) {
	m, server := newTestModule(t)
	offer := simulcastTestOffer("a=rid:h send\r\na=simulcast:send h\r\n")
	req := httptest.NewRequest(http.MethodPost, "/webrtc/whip/live/disabled", bytes.NewBufferString(offer))
	req.Header.Set("Content-Type", "application/sdp")
	res := httptest.NewRecorder()
	m.httpSrv.Handler.ServeHTTP(res, req)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status %d: %s", res.Code, res.Body.String())
	}
	if _, ok := server.StreamHub().Find("live/disabled"); ok {
		t.Fatal("unsupported offer allocated stream")
	}
	m.sessions.Range(func(_, _ any) bool { t.Error("unsupported offer allocated session"); return true })
}

func TestWHEPSimulcastWakesSelectedLayerBeforeStartup(t *testing.T) {
	m, server := newTestModule(t)
	stream, err := server.StreamHub().GetOrCreate("live/wake")
	if err != nil {
		t.Fatal(err)
	}
	pub := &authorizationTestPublisher{id: "wake", info: &avframe.MediaInfo{VideoCodec: avframe.CodecVP8}}
	if publisherErr := stream.SetPublisher(pub); publisherErr != nil {
		t.Fatal(publisherErr)
	}
	family, err := stream.AttachSimulcast(pub, simulcastTestConfig(true).Layers, true)
	if err != nil {
		t.Fatal(err)
	}
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	if _, transceiverErr := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); transceiverErr != nil {
		t.Fatal(transceiverErr)
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, selector := range []string{"unknown", "low"} {
		req := httptest.NewRequest(http.MethodPost, "/webrtc/whep/live/wake?layer="+selector, strings.NewReader(offer.SDP))
		req.Header.Set("Content-Type", "application/sdp")
		res := httptest.NewRecorder()
		finished := make(chan struct{})
		go func() { m.httpSrv.Handler.ServeHTTP(res, req); close(finished) }()
		if selector == "low" {
			deadline := time.Now().Add(time.Second)
			for time.Now().Before(deadline) {
				if active, _ := family.Processing("l"); active {
					break
				}
				time.Sleep(time.Millisecond)
			}
			active, _ := family.Processing("l")
			if !active {
				t.Error("selected suspended layer was not admitted before startup")
			}
			family.WriteVideo("l", avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecVP8, avframe.FrameTypeKeyframe, 0, 0, []byte{0, 1}))
		}
		select {
		case <-finished:
		case <-time.After(10 * time.Second):
			t.Fatal("WHEP setup did not finish")
		}
		if selector == "unknown" {
			if res.Code != http.StatusBadRequest {
				t.Fatalf("unknown layer: %d %s", res.Code, res.Body.String())
			}
		} else if res.Code != http.StatusCreated {
			t.Fatalf("low layer: %d %s", res.Code, res.Body.String())
		}
	}
}

func TestWHEPSimulcastStartupCannotCrossAcquiredGeneration(t *testing.T) {
	_, server := newTestModule(t)
	parent, err := server.StreamHub().GetOrCreate("live/generation")
	if err != nil {
		t.Fatal(err)
	}
	pub := &authorizationTestPublisher{id: "first", info: &avframe.MediaInfo{VideoCodec: avframe.CodecVP8}}
	if publisherErr := parent.SetPublisher(pub); publisherErr != nil {
		t.Fatal(publisherErr)
	}
	family, err := parent.AttachSimulcast(pub, simulcastTestConfig(false).Layers, false)
	if err != nil {
		t.Fatal(err)
	}
	selected, _, release, err := family.Acquire("auto", "webrtc")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	parent.RemovePublisherIf(pub)
	replacement := &authorizationTestPublisher{id: "second", info: pub.info}
	if err := parent.SetPublisher(replacement); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := captureWHEPSimulcastStartup(parent, selected, family); ok {
		t.Fatal("old family lease accepted replacement startup")
	}
	if parent.TotalSubscribers() != 1 {
		t.Fatal("test did not retain old generation lease")
	}
}
