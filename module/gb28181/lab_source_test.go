package gb28181

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/internal/labmedia"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/codec/h264"
	mediar "github.com/im-pingo/liveforge/pkg/rtp"
)

// Decode the public request shape so a dropped source field exercises the old
// generated-media behavior instead of merely failing to compile.
func gbSourceRequest(t *testing.T, key string) LabSessionRequest {
	t.Helper()
	request := validGBLabRequest()
	encoded, err := json.Marshal(map[string]string{"source_stream_key": key})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &request); err != nil {
		t.Fatal(err)
	}
	return request
}

func gbCameraFixture(t *testing.T, h realGBLabHarness, ready bool) (*core.Stream, *gbLabSourcePublisher, *avframe.AVFrame, *avframe.AVFrame) {
	t.Helper()
	// The source IDR has distinct user-data SEI and audio bytes absent from the
	// lab generator; the browser-style frame carries no inline parameter sets.
	const marker = "camera-source-unique-content"
	sei := append([]byte{0, 0, 0, 1, 6, 5, byte(16 + len(marker))}, bytes.Repeat([]byte{0x43}, 16)...)
	sei = append(sei, marker...)
	sei = append(sei, 0x80)
	payload := sei
	for _, nal := range h264.ExtractNALUs(labmedia.VideoFrame(0).Payload) {
		if nal[0]&31 == 5 {
			payload = append(payload, 0, 0, 0, 1)
			payload = append(payload, nal...)
		}
	}
	video := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 900, 917, mediar.AnnexBToAVCC(payload))
	audio := avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecG711A, avframe.FrameTypeKeyframe, 913, 913, bytes.Repeat([]byte{0x29}, 160))
	pub := &gbLabSourcePublisher{id: "browser-camera", info: avframe.MediaInfo{VideoCodec: avframe.CodecH264, AudioCodec: avframe.CodecG711A, SampleRate: 8000, Channels: 1}}
	if ready {
		pub.info.VideoSequenceHeader = mediar.BuildAVCDecoderConfig(labmedia.VideoFrame(0).Payload)
	}
	stream, err := h.hub.GetOrCreate("browser/camera")
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.SetPublisher(pub); err != nil {
		t.Fatal(err)
	}
	if ready {
		stream.WriteFrame(video)
		stream.WriteFrame(audio)
	}
	return stream, pub, video, audio
}

func TestGBLabPublishSourcePreservesMediaThroughRealIngress(t *testing.T) {
	h := newRealGBLabHarness(t)
	source, publisher, video, audio := gbCameraFixture(t, h, true)
	request := gbSourceRequest(t, source.Key())
	snapshot, err := h.module.StartLabSession(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	lab := h.module.labs.session(snapshot.ID)
	rtp, rtcp := lab.mediaAddresses()
	addresses := []string{lab.peerAddress(), rtp, rtcp}
	encoded, _ := json.Marshal(snapshot)
	if !bytes.Contains(encoded, []byte(`"source_stream_key":"browser/camera"`)) {
		t.Errorf("snapshot omitted source identity: %s", encoded)
	}
	target, ok := h.hub.Find(request.StreamKey)
	if !ok {
		t.Fatal("real INVITE did not create target")
	}
	reader := target.RingBuffer().NewReader()
	defer reader.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var sawVideo, sawAudio, sawHeader bool
	for !sawVideo || !sawAudio || !sawHeader {
		frame, ok := reader.ReadContext(ctx)
		if !ok {
			t.Fatalf("source content not received through PS/RTP: video=%v audio=%v header=%v", sawVideo, sawAudio, sawHeader)
		}
		if frame.MediaType.IsVideo() && frame.FrameType == avframe.FrameTypeSequenceHeader {
			sawHeader = true
		}
		if frame.MediaType.IsVideo() && bytes.Contains(frame.Payload, []byte("camera-source-unique-content")) {
			sawVideo = frame.DTS == video.DTS && frame.PTS == video.PTS
		}
		if frame.MediaType.IsAudio() && bytes.Equal(frame.Payload, audio.Payload) {
			sawAudio = frame.DTS == audio.DTS
		}
	}
	if !lab.inviteReceived.Load() || !lab.ackReceived.Load() || !lab.catalogSent.Load() {
		t.Fatal("source bypassed normal REGISTER/Catalog/INVITE/ACK")
	}
	if source.Subscribers()["gb28181"] != 1 {
		t.Fatalf("source subscriber count = %v", source.Subscribers())
	}
	// A subsequent live audio frame must traverse the reader as well as replay.
	live := avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecG711A, avframe.FrameTypeKeyframe, 933, 933, bytes.Repeat([]byte{0x37}, 160))
	source.WriteFrame(live)
	for {
		frame, ok := reader.ReadContext(ctx)
		if !ok {
			t.Fatal("live source audio missing")
		}
		if frame.MediaType.IsAudio() && bytes.Equal(frame.Payload, live.Payload) {
			break
		}
	}
	if err := h.module.StopLabSession(snapshot.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.module.StopLabSession(snapshot.ID); err != nil {
		t.Fatal(err)
	}
	if source.Publisher() != publisher || source.Subscribers()["gb28181"] != 0 {
		t.Fatal("stop leaked source ownership or removed camera publisher")
	}
	if target.Publisher() != nil || len(h.module.sessions.All()) != 0 {
		t.Fatal("stop leaked protocol target")
	}
	for _, addr := range addresses {
		assertGBLabPortFree(t, addr)
	}
}

func TestGBLabPublishSourceRejectsInvalidBeforeSignaling(t *testing.T) {
	for _, test := range []struct {
		name, key    string
		mode         LabMode
		video, audio avframe.CodecType
	}{
		{name: "missing", key: "missing/source"}, {name: "same", key: "gb28181/lab"}, {name: "invalid", key: "browser/../camera"},
		{name: "receive", key: "browser/camera", mode: LabModeReceive}, {name: "video", key: "browser/camera", video: avframe.CodecVP8, audio: avframe.CodecG711A},
		{name: "audio", key: "browser/camera", video: avframe.CodecH264, audio: avframe.CodecOpus},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newRealGBLabHarness(t)
			source, _, _, _ := gbCameraFixture(t, h, true)
			if test.video != 0 {
				source.RemovePublisher()
				if err := source.SetPublisher(&gbLabSourcePublisher{id: "incompatible", info: avframe.MediaInfo{VideoCodec: test.video, AudioCodec: test.audio}}); err != nil {
					t.Fatal(err)
				}
			}
			request := gbSourceRequest(t, test.key)
			if test.mode != "" {
				request.Mode = test.mode
			}
			snapshot, err := h.module.StartLabSession(context.Background(), request)
			if !errors.Is(err, ErrLabInvalidRequest) {
				t.Fatalf("Start = %+v, %v; want invalid request", snapshot, err)
			}
			if h.module.registry.Get(request.DeviceID) != nil || len(h.module.sessions.All()) != 0 {
				t.Fatal("invalid source signaled")
			}
			if lab := h.module.labs.session(snapshot.ID); lab != nil && lab.peerAddress() != "" {
				t.Fatal("invalid source allocated signaling sockets")
			}
		})
	}
}

func TestGBLabPublishSourceSubscriberAdmissionPrecedesSignaling(t *testing.T) {
	h := newRealGBLabHarnessWithConfig(t, func(cfg *config.Config) { cfg.Limits.MaxSubscribersPerStream = 1 })
	source, _, _, _ := gbCameraFixture(t, h, true)
	if err := source.AddSubscriber("occupied"); err != nil {
		t.Fatal(err)
	}
	defer source.RemoveSubscriber("occupied")
	request := gbSourceRequest(t, source.Key())
	snapshot, err := h.module.StartLabSession(context.Background(), request)
	if err == nil || !strings.Contains(err.Error(), "max subscribers") {
		t.Fatalf("Start = %+v, %v; want admission failure", snapshot, err)
	}
	if h.module.registry.Get(request.DeviceID) != nil || len(h.module.sessions.All()) != 0 || source.Subscribers()["gb28181"] != 0 {
		t.Fatal("rejected source leaked resources")
	}
}

func TestGBLabPublishSourceRetirementFailsAndReleases(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(map[bool]string{false: "end", true: "replace"}[replace], func(t *testing.T) {
			h := newRealGBLabHarness(t)
			source, _, _, _ := gbCameraFixture(t, h, true)
			snapshot, err := h.module.StartLabSession(context.Background(), gbSourceRequest(t, source.Key()))
			if err != nil {
				t.Fatal(err)
			}
			lab := h.module.labs.session(snapshot.ID)
			rtp, rtcp := lab.mediaAddresses()
			peer := lab.peerAddress()
			waitForGBLabSnapshot(t, h.module, snapshot.ID, func(s LabSessionSnapshot) bool { return s.AudioFramesSent > 0 })
			source.RemovePublisher()
			if replace {
				if err := source.SetPublisher(&gbLabSourcePublisher{id: "replacement", info: avframe.MediaInfo{VideoCodec: avframe.CodecH264, AudioCodec: avframe.CodecG711A, VideoSequenceHeader: labmedia.VideoFrame(0).Payload}}); err != nil {
					t.Fatal(err)
				}
				source.WriteFrame(labmedia.VideoFrame(0))
			}
			failed := waitForGBLabSnapshot(t, h.module, snapshot.ID, func(s LabSessionSnapshot) bool { return s.State == LabSessionStateFailed })
			if failed.LastError == "" || source.Subscribers()["gb28181"] != 0 || len(h.module.sessions.All()) != 0 {
				t.Fatalf("retirement left ownership: %+v", failed)
			}
			for _, addr := range []string{rtp, rtcp, peer} {
				assertGBLabPortFree(t, addr)
			}
			if replace && (source.Publisher() == nil || source.Publisher().ID() != "replacement") {
				t.Fatal("retirement removed replacement")
			}
		})
	}
}

func TestGBLabPublishSourceStartupCancellationAndReplacement(t *testing.T) {
	for _, action := range []string{"caller", "stop", "replacement", "headers"} {
		t.Run(action, func(t *testing.T) {
			h := newRealGBLabHarness(t)
			source, _, video, audio := gbCameraFixture(t, h, false)
			request := gbSourceRequest(t, source.Key())
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			type result struct {
				snapshot LabSessionSnapshot
				err      error
			}
			done := make(chan result, 1)
			go func() { snapshot, err := h.module.StartLabSession(ctx, request); done <- result{snapshot, err} }()
			var pending LabSessionSnapshot
			waitForGBLab(t, func() bool {
				sessions := h.module.ListLabSessions()
				if len(sessions) == 0 {
					return false
				}
				pending = sessions[0]
				return true
			})
			select {
			case r := <-done:
				t.Fatalf("unready source completed setup: %+v", r)
			case <-time.After(30 * time.Millisecond):
			}
			if h.module.registry.Get(request.DeviceID) != nil || h.module.labs.session(pending.ID).peerAddress() != "" {
				t.Fatal("source startup signaled before headers")
			}
			switch action {
			case "caller":
				cancel()
			case "stop":
				stopped := make(chan error, 1)
				go func() { stopped <- h.module.StopLabSession(pending.ID) }()
				select {
				case err := <-stopped:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(time.Second):
					t.Fatal("stop did not cancel source startup")
				}
			case "replacement":
				source.RemovePublisher()
				if err := source.SetPublisher(&gbLabSourcePublisher{id: "next-camera", info: avframe.MediaInfo{VideoCodec: avframe.CodecH264, AudioCodec: avframe.CodecG711A, VideoSequenceHeader: labmedia.VideoFrame(0).Payload}}); err != nil {
					t.Fatal(err)
				}
			case "headers":
				source.WriteFrame(avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader, 0, 0, mediar.BuildAVCDecoderConfig(labmedia.VideoFrame(0).Payload)))
				source.WriteFrame(video)
				source.WriteFrame(audio)
			}
			select {
			case r := <-done:
				if action == "headers" {
					if r.err != nil {
						t.Fatalf("late header setup failed: %v", r.err)
					}
					if err := h.module.StopLabSession(r.snapshot.ID); err != nil {
						t.Fatal(err)
					}
				} else if r.err == nil {
					t.Fatalf("canceled startup attached or activated: %+v", r.snapshot)
				}
			case <-time.After(time.Second):
				t.Fatal("source startup ignored cancellation or late header")
			}
			if source.Subscribers()["gb28181"] != 0 || len(h.module.sessions.All()) != 0 || h.module.registry.Get(request.DeviceID) != nil {
				t.Fatal("startup cleanup leaked subscriber or dialog")
			}
		})
	}
}

func TestGBLabPublishSourceOverwriteDiscardsRetainedValue(t *testing.T) {
	h := newRealGBLabHarnessWithConfig(t, func(cfg *config.Config) { cfg.Stream.RingBufferSize = 4 })
	source, _, _, _ := gbCameraFixture(t, h, true)
	lab := newGBLabSession("overwrite", "overwrite", gbSourceRequest(t, source.Key()), h.module)
	captured, err := lab.preparePublishSource(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	lab.source = captured
	defer captured.close()
	// Remove replay from this focused reader test; only retained post-gap values
	// exist, so any successful media send would prove a continuity violation.
	captured.snapshot.ReplayFrames = nil
	if err := lab.openMediaPair(); err != nil {
		t.Fatal(err)
	}
	defer lab.rtpConn.Close()
	defer lab.rtcpConn.Close()
	for i := 0; i < 8; i++ {
		source.WriteFrame(avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecG711A, avframe.FrameTypeKeyframe, int64(i*20), int64(i*20), bytes.Repeat([]byte{0x39}, 160)))
	}
	done := make(chan error, 1)
	go func() { done <- lab.publishSourceMediaLoop(9) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "overwritten") {
			t.Fatalf("source overwrite = %v", err)
		}
	case <-time.After(time.Second):
		lab.cancel()
		t.Fatal("overwritten source did not terminate")
	}
	if lab.rtpPacketsSent.Load() != 0 || source.Subscribers()["gb28181"] != 0 {
		t.Fatal("post-gap value sent or source subscriber leaked")
	}
}

func TestGBLabPublishSourceReaderEOFFailsSession(t *testing.T) {
	h := newRealGBLabHarness(t)
	source, _, _, _ := gbCameraFixture(t, h, true)
	snapshot, err := h.module.StartLabSession(context.Background(), gbSourceRequest(t, source.Key()))
	if err != nil {
		t.Fatal(err)
	}
	waitForGBLabSnapshot(t, h.module, snapshot.ID, func(s LabSessionSnapshot) bool { return s.AudioFramesSent > 0 })
	source.RingBuffer().Close()
	failed := waitForGBLabSnapshot(t, h.module, snapshot.ID, func(s LabSessionSnapshot) bool { return s.State == LabSessionStateFailed })
	if !strings.Contains(failed.LastError, "reader ended") || source.Subscribers()["gb28181"] != 0 || len(h.module.sessions.All()) != 0 {
		t.Fatalf("source EOF did not clean up: %+v", failed)
	}
}

func TestGBLabPublishSourceStartupHasFiniteBudget(t *testing.T) {
	h := newRealGBLabHarness(t)
	source, _, _, _ := gbCameraFixture(t, h, false)
	started := time.Now()
	snapshot, err := h.module.StartLabSession(context.Background(), gbSourceRequest(t, source.Key()))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unready source = %+v, %v; want bounded timeout", snapshot, err)
	}
	if time.Since(started) > 6*time.Second {
		t.Fatal("startup exceeded the bounded media admission budget")
	}
	lab := h.module.labs.session(snapshot.ID)
	if lab.peerAddress() != "" || source.Subscribers()["gb28181"] != 0 || len(h.module.sessions.All()) != 0 {
		t.Fatal("unready source allocated protocol resources")
	}
}

func TestGBLabPublishSourceTerminalHistoryDropsMediaReferences(t *testing.T) {
	for _, reason := range []string{"stop", "source_failure"} {
		t.Run(reason, func(t *testing.T) {
			h := newRealGBLabHarness(t)
			source, _, _, _ := gbCameraFixture(t, h, true)
			snapshot, err := h.module.StartLabSession(context.Background(), gbSourceRequest(t, source.Key()))
			if err != nil {
				t.Fatal(err)
			}
			lab := h.module.labs.session(snapshot.ID)
			captured := lab.source
			waitForGBLabSnapshot(t, h.module, snapshot.ID, func(s LabSessionSnapshot) bool { return s.AudioFramesSent > 0 })
			if reason == "stop" {
				if err := h.module.StopLabSession(snapshot.ID); err != nil {
					t.Fatal(err)
				}
			} else {
				source.RemovePublisher()
				waitForGBLabSnapshot(t, h.module, snapshot.ID, func(s LabSessionSnapshot) bool { return s.State == LabSessionStateFailed })
			}
			if h.module.labs.session(snapshot.ID) != lab {
				t.Fatal("terminal history unexpectedly discarded the session record")
			}
			if captured.stream != nil || captured.reader != nil || captured.release != nil || captured.parameters != nil {
				t.Error("terminal history retains the source stream, reader, release closure, or codec parameters")
			}
			startup := captured.snapshot
			if startup.ReplayFrames != nil || startup.VideoSequenceHeader != nil || startup.AudioSequenceHeader != nil || startup.MediaInfo.VideoSequenceHeader != nil || startup.MediaInfo.AudioSequenceHeader != nil || startup.GenerationDone != nil {
				t.Error("terminal history retains source replay frames, decoder headers, or generation state")
			}
			if source.Subscribers()["gb28181"] != 0 {
				t.Fatal("terminal history still owns a source subscriber")
			}
			if err := h.module.StopLabSession(snapshot.ID); reason == "stop" && err != nil {
				t.Fatal(err)
			}
		})
	}
}
