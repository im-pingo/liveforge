package sipgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/internal/labmedia"
	"github.com/im-pingo/liveforge/pkg/avframe"
)

func sipSourceRequest(t *testing.T, source, codec string) LabSessionRequest {
	t.Helper()
	data, err := json.Marshal(map[string]string{
		"mode": "publish", "device_id": "camera-device", "stream_key": "sip/camera-target",
		"source_stream_key": source, "codec": codec,
	})
	if err != nil {
		t.Fatal(err)
	}
	var request LabSessionRequest
	if err := json.Unmarshal(data, &request); err != nil {
		t.Fatal(err)
	}
	return request
}

func newSIPCameraSource(t *testing.T, hub *core.StreamHub, info *avframe.MediaInfo) (*core.Stream, *gatewayTestPublisher) {
	t.Helper()
	stream, err := hub.GetOrCreate("capture/camera")
	if err != nil {
		t.Fatal(err)
	}
	publisher := &gatewayTestPublisher{id: "browser-camera", info: info}
	if err := stream.SetPublisher(publisher); err != nil {
		t.Fatal(err)
	}
	return stream, publisher
}

func sipCameraInfo(codec avframe.CodecType) *avframe.MediaInfo {
	return &avframe.MediaInfo{VideoCodec: avframe.CodecH264, VideoSequenceHeader: labmedia.VideoFrame(0).Payload,
		AudioCodec: codec, SampleRate: 8000, Channels: 1}
}

func TestSIPLabPublishSourceContentTraversesRTPAndStopReleasesSource(t *testing.T) {
	for _, codec := range []string{"PCMA", "PCMU"} {
		t.Run(codec, func(t *testing.T) {
			h := newRealSIPLabHarness(t)
			source, publisher := newSIPCameraSource(t, h.hub, sipCameraInfo(codecAV(codec)))
			video := []byte{0x65, 0x88, 0x84, 0xde, 0xad, 0x42}
			audio := bytes.Repeat([]byte{0x72, 0x19, 0x3a, 0x91}, 40)
			source.WriteFrame(avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 1234, 1234, video))
			source.WriteFrame(avframe.NewAVFrame(avframe.MediaTypeAudio, codecAV(codec), avframe.FrameTypeInterframe, 1234, 1234, audio))
			source.WriteFrame(avframe.NewAVFrame(avframe.MediaTypeAudio, codecAV(codec), avframe.FrameTypeInterframe, 1294, 1294, audio))
			session, err := h.module.StartLabSession(context.Background(), sipSourceRequest(t, source.Key(), codec))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = h.module.StopLabSession(session.ID) })
			target, ok := h.hub.Find(session.StreamKey)
			if !ok {
				t.Fatal("source-backed publish did not create the target")
			}
			reader := target.RingBuffer().NewReaderAt(0)
			defer reader.Close()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			var gotVideo, gotHeader bool
			var audioDTS []int64
			for !gotVideo || !gotHeader || len(audioDTS) != 2 {
				frame, ok := reader.ReadContext(ctx)
				if !ok {
					t.Fatalf("actual source content missing: video=%v header=%v audio timestamps=%v", gotVideo, gotHeader, audioDTS)
				}
				if frame.MediaType.IsVideo() && frame.FrameType == avframe.FrameTypeSequenceHeader {
					gotHeader = true
				}
				if frame.MediaType.IsVideo() && bytes.Contains(frame.Payload, video) {
					gotVideo = true
					if frame.DTS != 1234 {
						t.Fatalf("video DTS = %d, want 1234", frame.DTS)
					}
				}
				if frame.MediaType.IsAudio() && bytes.Equal(frame.Payload, audio) {
					audioDTS = append(audioDTS, frame.DTS)
				}
			}
			if audioDTS[1]-audioDTS[0] != 60 {
				t.Fatalf("source audio spacing changed: %v", audioDTS)
			}
			lab, call := activeSIPLabCall(t, h.module.Gateway(), session.ID)
			addresses := sipLabMediaAddresses(lab)
			if call.snapshot().Direction != "inbound" {
				t.Fatal("camera source bypassed inbound SIP")
			}
			waitForSIPLabSnapshot(t, h.module, session.ID, func(s LabSessionSnapshot) bool {
				return s.AudioRTPPacketsSent >= 2 && s.VideoRTPPacketsSent > 0 && s.RTCPPacketsSent > 0
			})
			if source.TotalSubscribers() != 1 {
				t.Fatalf("source subscribers = %d, want 1", source.TotalSubscribers())
			}
			encoded, err := json.Marshal(lab.snapshot())
			if err != nil || !bytes.Contains(encoded, []byte(`"source_stream_key":"capture/camera"`)) {
				t.Fatalf("source key absent from snapshot: %s, %v", encoded, err)
			}
			if err := h.module.StopLabSession(session.ID); err != nil {
				t.Fatal(err)
			}
			if source.TotalSubscribers() != 0 || source.Publisher() != publisher {
				t.Fatal("stop leaked source ownership or changed its publisher")
			}
			if lab.source.stream != nil || lab.source.reader != nil || len(lab.source.snapshot.ReplayFrames) != 0 {
				t.Fatal("terminal lab retained source stream, reader, or captured replay frames")
			}
			if target.Publisher() != nil || h.module.Gateway().ActiveCalls() != 0 {
				t.Fatal("stop left the protocol target active")
			}
			for _, address := range addresses {
				assertSIPLabPortFree(t, address)
			}
		})
	}
}

func TestSIPLabPublishSourceValidationBeforeSignaling(t *testing.T) {
	for _, test := range []struct {
		name, source string
		info         *avframe.MediaInfo
		want         error
		receive      bool
	}{
		{name: "missing", source: "capture/missing", want: ErrStreamNotFound},
		{name: "invalid key", source: "capture/../camera", want: ErrLabInvalidRequest},
		{name: "same key", source: "sip/camera-target", want: ErrLabInvalidRequest},
		{name: "receive mode", source: "capture/camera", want: ErrLabInvalidRequest, receive: true},
		{name: "video codec", source: "capture/camera", info: &avframe.MediaInfo{VideoCodec: avframe.CodecVP8, AudioCodec: avframe.CodecG711A}, want: ErrCodecMismatch},
		{name: "audio codec", source: "capture/camera", info: sipCameraInfo(avframe.CodecOpus), want: ErrCodecMismatch},
		{name: "other G711", source: "capture/camera", info: sipCameraInfo(avframe.CodecG711U), want: ErrCodecMismatch},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newRealSIPLabHarness(t)
			if test.info != nil {
				newSIPCameraSource(t, h.hub, test.info)
			}
			request := sipSourceRequest(t, test.source, "PCMA")
			if test.receive {
				request.Mode = LabModeReceive
			}
			_, err := h.module.StartLabSession(context.Background(), request)
			if !errors.Is(err, test.want) {
				t.Fatalf("Start error = %v, want %v", err, test.want)
			}
			assertSIPSourceRejectedBeforeSignaling(t, h.module.Gateway())
		})
	}
}

func assertSIPSourceRejectedBeforeSignaling(t *testing.T, gateway *Gateway) {
	t.Helper()
	if len(gateway.ListCalls()) != 0 {
		t.Fatal("invalid source created a SIP call")
	}
	gateway.labs.mu.RLock()
	defer gateway.labs.mu.RUnlock()
	for _, lab := range gateway.labs.sessions {
		if len(sipLabMediaAddresses(lab)) != 0 {
			t.Fatal("source rejection allocated protocol sockets")
		}
	}
}

func TestSIPLabPublishSourceAdmissionRejectsBeforeSignaling(t *testing.T) {
	h := newRealSIPLabHarness(t)
	hub := core.NewStreamHub(config.StreamConfig{RingBufferSize: 16}, config.LimitsConfig{MaxSubscribersPerStream: 1}, core.NewEventBus())
	h.module.Gateway().hub = hub
	source, _ := newSIPCameraSource(t, hub, sipCameraInfo(avframe.CodecG711A))
	if err := source.AddSubscriber("occupied"); err != nil {
		t.Fatal(err)
	}
	defer source.RemoveSubscriber("occupied")
	_, err := h.module.StartLabSession(context.Background(), sipSourceRequest(t, source.Key(), "PCMA"))
	if err == nil || !strings.Contains(err.Error(), "subscriber") {
		t.Fatalf("admission error = %v", err)
	}
	assertSIPSourceRejectedBeforeSignaling(t, h.module.Gateway())
	if source.TotalSubscribers() != 1 {
		t.Fatal("rejected admission leaked a subscriber")
	}
}

func TestSIPLabPublishSourceStartupCancellation(t *testing.T) {
	h := newRealSIPLabHarness(t)
	info := sipCameraInfo(avframe.CodecG711A)
	info.VideoSequenceHeader = nil
	source, _ := newSIPCameraSource(t, h.hub, info)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	_, err := h.module.StartLabSession(ctx, sipSourceRequest(t, source.Key(), "PCMA"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("startup cancellation error = %v", err)
	}
	assertSIPSourceRejectedBeforeSignaling(t, h.module.Gateway())
	if source.TotalSubscribers() != 0 {
		t.Fatal("startup cancellation leaked subscriber")
	}
}

func TestSIPLabPublishSourceWaitsForBothBrowserTracks(t *testing.T) {
	h := newRealSIPLabHarness(t)
	source, _ := newSIPCameraSource(t, h.hub, &avframe.MediaInfo{AudioCodec: avframe.CodecG711A, SampleRate: 8000, Channels: 1})
	request := sipSourceRequest(t, source.Key(), "PCMA")
	type startResult struct {
		snapshot LabSessionSnapshot
		err      error
	}
	result := make(chan startResult, 1)
	go func() {
		snapshot, err := h.module.StartLabSession(context.Background(), request)
		result <- startResult{snapshot, err}
	}()
	select {
	case started := <-result:
		t.Fatalf("source setup finished before browser video arrived: %+v", started)
	case <-time.After(50 * time.Millisecond):
	}
	header := labmedia.VideoFrame(0)
	header.FrameType = avframe.FrameTypeSequenceHeader
	source.WriteFrame(header)
	select {
	case started := <-result:
		if started.err != nil {
			t.Fatal(started.err)
		}
		if err := h.module.StopLabSession(started.snapshot.ID); err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("source setup did not resume after browser video arrived")
	}
}

func TestSIPLabPublishSourceStopCancelsStartupWait(t *testing.T) {
	h := newRealSIPLabHarness(t)
	info := sipCameraInfo(avframe.CodecG711A)
	info.VideoSequenceHeader = nil
	source, _ := newSIPCameraSource(t, h.hub, info)
	result := make(chan error, 1)
	go func() {
		_, err := h.module.StartLabSession(context.Background(), sipSourceRequest(t, source.Key(), "PCMA"))
		result <- err
	}()
	var id string
	waitForSIPLab(t, func() bool {
		for _, snapshot := range h.module.ListLabSessions() {
			if snapshot.State == LabSessionStateStarting {
				id = snapshot.ID
				return true
			}
		}
		return false
	})
	stopped := make(chan error, 1)
	go func() { stopped <- h.module.StopLabSession(id) }()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Stop deadlocked during source startup")
	}
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("startup error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("source startup did not exit")
	}
	assertSIPSourceRejectedBeforeSignaling(t, h.module.Gateway())
}

func TestSIPLabPublishSourceRetirementFailsAndReleasesTransport(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(map[bool]string{false: "EOF", true: "replacement"}[replace], func(t *testing.T) {
			h := newRealSIPLabHarness(t)
			source, _ := newSIPCameraSource(t, h.hub, sipCameraInfo(avframe.CodecG711A))
			session, err := h.module.StartLabSession(context.Background(), sipSourceRequest(t, source.Key(), "PCMA"))
			if err != nil {
				t.Fatal(err)
			}
			lab, _ := activeSIPLabCall(t, h.module.Gateway(), session.ID)
			addresses := sipLabMediaAddresses(lab)
			source.RemovePublisher()
			var replacement *gatewayTestPublisher
			if replace {
				replacement = &gatewayTestPublisher{id: "replacement-camera", info: sipCameraInfo(avframe.CodecG711A)}
				if err := source.SetPublisher(replacement); err != nil {
					t.Fatal(err)
				}
				source.WriteFrame(labmedia.VideoFrame(0))
			}
			failed := waitForSIPLabSnapshot(t, h.module, session.ID, func(s LabSessionSnapshot) bool { return s.State == LabSessionStateFailed })
			if !strings.Contains(failed.LastError, "source") {
				t.Fatalf("failure omitted source diagnosis: %+v", failed)
			}
			if source.TotalSubscribers() != 0 || h.module.Gateway().ActiveCalls() != 0 {
				t.Fatal("retired source leaked subscriber or call")
			}
			if replace && source.Publisher() != replacement {
				t.Fatal("source retirement affected replacement publisher")
			}
			for _, address := range addresses {
				assertSIPLabPortFree(t, address)
			}
		})
	}
}

func TestSIPLabPublishSourceOverwriteFailsWithoutSendingRetainedFrame(t *testing.T) {
	h := newRealSIPLabHarness(t)
	source, publisher := newSIPCameraSource(t, h.hub, sipCameraInfo(avframe.CodecG711A))
	request := sipSourceRequest(t, source.Key(), "PCMA")
	lab := newSIPLabSession("overwrite", request.DeviceID, request, h.module.Gateway())
	var err error
	lab.source, err = lab.admitPublishSource(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lab.stop() })
	// Capture the source cursor before a real ring overflow. The worker must
	// discard ReadResult's retained post-gap value and retire itself before
	// failure cleanup waits for the media wait group.
	for i := 0; i < 300; i++ {
		source.WriteFrame(labmedia.G711Frame(avframe.CodecG711A, int64(i)*20))
	}
	close(lab.startDone)
	lab.mediaWG.Add(1)
	done := make(chan struct{})
	go func() { lab.publishSourceLoop(0, 0); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("source overwrite failure deadlocked with its sender")
	}
	snapshot := lab.snapshot()
	if snapshot.State != LabSessionStateFailed || !strings.Contains(snapshot.LastError, "overwritten") {
		t.Fatalf("overwrite result = %+v", snapshot)
	}
	if snapshot.RTPPacketsSent != 0 || snapshot.RTCPPacketsSent != 0 {
		t.Fatal("source overwrite forwarded retained media")
	}
	if source.TotalSubscribers() != 0 || source.Publisher() != publisher {
		t.Fatal("source overwrite did not release its own subscription")
	}
}

func TestSIPLabPublishSourceGatesVideoUntilKeyframe(t *testing.T) {
	h := newRealSIPLabHarness(t)
	source, _ := newSIPCameraSource(t, h.hub, sipCameraInfo(avframe.CodecG711A))
	session, err := h.module.StartLabSession(context.Background(), sipSourceRequest(t, source.Key(), "PCMA"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.module.StopLabSession(session.ID) })
	source.WriteFrame(avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe, 0, 0, []byte{0x41, 0xee, 0x19, 0x23}))
	source.WriteFrame(labmedia.G711Frame(avframe.CodecG711A, 0))
	waitForSIPLabSnapshot(t, h.module, session.ID, func(s LabSessionSnapshot) bool { return s.AudioRTPPacketsSent > 0 })
	lab, _ := activeSIPLabCall(t, h.module.Gateway(), session.ID)
	if lab.snapshot().VideoRTPPacketsSent != 0 {
		t.Fatal("source video started on an interframe")
	}
	source.WriteFrame(avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 40, 40, []byte{0x65, 0x88, 0x84, 0x56}))
	waitForSIPLabSnapshot(t, h.module, session.ID, func(s LabSessionSnapshot) bool { return s.VideoRTPPacketsSent >= 3 })
	target, _ := h.hub.Find(session.StreamKey)
	waitForSIPLab(t, func() bool { return target.StartupSnapshot().VideoSequenceHeader != nil })
}

func TestSIPLabPublishSourceStartupHasBoundedWait(t *testing.T) {
	h := newRealSIPLabHarness(t)
	source, _ := newSIPCameraSource(t, h.hub, &avframe.MediaInfo{})
	started := time.Now()
	_, err := h.module.StartLabSession(context.Background(), sipSourceRequest(t, source.Key(), "PCMA"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("startup timeout = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 6*time.Second {
		t.Fatalf("startup took %s, want at most 6s", elapsed)
	}
	assertSIPSourceRejectedBeforeSignaling(t, h.module.Gateway())
}

func TestSIPLabPublishSourceFailuresKeepTerminalHistoryBounded(t *testing.T) {
	h := newRealSIPLabHarness(t)
	source, _ := newSIPCameraSource(t, h.hub, sipCameraInfo(avframe.CodecG711A))
	for i := 0; i < sipLabTerminalHistoryLimit+2; i++ {
		request := sipSourceRequest(t, source.Key(), "PCMA")
		request.DeviceID = fmt.Sprintf("source-failure-%d", i)
		session, err := h.module.StartLabSession(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		source.RemovePublisher()
		waitForSIPLabSnapshot(t, h.module, session.ID, func(s LabSessionSnapshot) bool { return s.State == LabSessionStateFailed })
		if err := source.SetPublisher(&gatewayTestPublisher{id: fmt.Sprintf("source-generation-%d", i), info: sipCameraInfo(avframe.CodecG711A)}); err != nil {
			t.Fatal(err)
		}
	}
	if count := len(h.module.ListLabSessions()); count != sipLabTerminalHistoryLimit {
		t.Fatalf("source failures retained %d terminal sessions, want %d", count, sipLabTerminalHistoryLimit)
	}
}
