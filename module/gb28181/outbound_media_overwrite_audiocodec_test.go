//go:build audiocodec

package gb28181

import (
	"context"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/audiocodec"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/muxer/ps"
	"github.com/im-pingo/liveforge/pkg/util"
)

func TestGBOutboundTranscodedReplayTargetOverwriteDropsRetainedAudioAtLiveBoundary(t *testing.T) {
	events, pauses := installGBOverwriteLogObserver(t, true)
	h := newGBDualOverwriteHarness(t, "gb28181/transcoded-replay-overwrite")
	audio := util.NewRingBuffer[*avframe.AVFrame](2)
	audioReader := audio.NewReaderAt(0)
	t.Cleanup(audioReader.Close)
	for _, frame := range []*avframe.AVFrame{
		gbOverwriteAudio(0xa1, 0),
		gbOverwriteAudio(0xa2, 20),
		gbOverwriteAudio(0xa3, 40),
		gbOverwriteAudio(0xa4, 60),
	} {
		audio.Write(frame)
	}
	replay := []*avframe.AVFrame{
		gbOverwriteVideo(avframe.FrameTypeKeyframe, 0xb0, 0),
		gbOverwriteVideo(avframe.FrameTypeInterframe, 0xb1, 40),
	}
	type replayResult struct {
		state gbTranscodedReplayState
		err   error
	}
	done := make(chan replayResult, 1)
	go func() {
		state, err := h.sender.sendTranscodedReplay(context.Background(), ps.NewMuxer(), replay, audioReader)
		done <- replayResult{state: state, err: err}
	}()

	select {
	case got := <-events:
		want := gbOverwriteLogEvent{reader: "target_audio", action: "continue_audio", overwritten: 2}
		if got != want {
			t.Fatalf("overwrite event = %+v, want %+v", got, want)
		}
	case result := <-done:
		t.Fatalf("replay returned without reporting target overwrite: state=%+v err=%v", result.state, result.err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for replay target-audio overwrite")
	}
	audio.Write(gbOverwriteAudio(0xc0, 80))
	close(<-pauses)

	var result replayResult
	select {
	case result = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("transcoded replay did not cross to live audio after overwrite")
	}
	if result.err != nil {
		t.Fatalf("sendTranscodedReplay: %v", result.err)
	}
	if result.state.pendingAudio == nil || !slices.Contains(result.state.pendingAudio.Payload, byte(0xc0)) {
		t.Fatalf("pending live audio = %#v, want post-gap marker c0", result.state.pendingAudio)
	}
	assertGBPSPack(t, h.capture.readPack(t), avframe.MediaTypeVideo, 0xb0, 0)
	assertGBPSPack(t, h.capture.readPack(t), avframe.MediaTypeVideo, 0xb1, 40)
	assertNoGBRTPPacket(t, h.capture.conn)
}

func TestGBOutboundTranscodedReplayActiveTargetAudioEOFFails(t *testing.T) {
	h := newGBDualOverwriteHarness(t, "gb28181/transcoded-replay-target-eof")
	audio := util.NewRingBuffer[*avframe.AVFrame](2)
	audioReader := audio.NewReaderAt(0)
	t.Cleanup(audioReader.Close)
	audio.Close()
	done := make(chan error, 1)
	go func() {
		_, err := h.sender.sendTranscodedReplay(
			context.Background(), ps.NewMuxer(),
			[]*avframe.AVFrame{gbOverwriteVideo(avframe.FrameTypeKeyframe, 0xa0, 0)},
			audioReader,
		)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "target audio ended during replay") {
			t.Fatalf("active replay target-audio EOF error = %v, want explicit failure", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("active replay target-audio EOF did not fail within the bound")
	}
	assertNoGBRTPPacket(t, h.capture.conn)
}

func TestGBOutboundTranscodedSourceOverwriteKeepsTargetAudioAndRecoversFreshVideo(t *testing.T) {
	events, pauses := installGBOverwriteLogObserver(t, true)
	h := newGBDualOverwriteHarness(t, "gb28181/transcoded-source-overwrite")
	source := util.NewRingBuffer[*avframe.AVFrame](2)
	audio := util.NewRingBuffer[*avframe.AVFrame](16)
	audio.Write(gbOverwriteAudio(0xb0, 100))
	for _, frame := range []*avframe.AVFrame{
		gbOverwriteVideo(avframe.FrameTypeInterframe, 0xa1, 20),
		gbOverwriteVideo(avframe.FrameTypeInterframe, 0xa2, 40),
		gbOverwriteVideo(avframe.FrameTypeInterframe, 0xa3, 60),
		gbOverwriteVideo(avframe.FrameTypeInterframe, 0xa4, 80),
	} {
		source.Write(frame)
	}

	var muxerCreations atomic.Int32
	targetStarted := make(chan struct{})
	releaseTarget := make(chan struct{})
	var releaseTargetOnce sync.Once
	t.Cleanup(func() { releaseTargetOnce.Do(func() { close(releaseTarget) }) })
	h.start(source.NewReaderAt(0), audio.NewReaderAt(0), func(sender *outboundMediaSession) {
		sender.newPSMuxer = func() *ps.Muxer {
			muxerCreations.Add(1)
			return ps.NewMuxer()
		}
		sender.pumpObserver = &gbMediaPumpObserver{started: func(reader gbMediaReader) {
			if reader == gbMediaReaderTargetAudio {
				close(targetStarted)
				<-releaseTarget
			}
		}}
	})
	assertGBOverwriteEvent(t, events, gbOverwriteLogEvent{
		reader: "source", action: "wait_keyframe", overwritten: 2,
	})
	waitGBSignal(t, targetStarted, "target-audio pump did not park before source overwrite")
	releaseTargetOnce.Do(func() { close(releaseTarget) })
	close(<-pauses)
	assertGBPSPack(t, h.capture.readPack(t), avframe.MediaTypeAudio, 0xb0, 100)
	source.Write(gbOverwriteVideo(avframe.FrameTypeInterframe, 0xb1, 120))
	audio.Write(gbOverwriteAudio(0xb2, 140))
	assertGBPSPack(t, h.capture.readPack(t), avframe.MediaTypeAudio, 0xb2, 140)
	source.Write(gbOverwriteVideoHeader(0xb3, 160))
	audio.Write(gbOverwriteAudio(0xb4, 180))
	assertGBPSPack(t, h.capture.readPack(t), avframe.MediaTypeAudio, 0xb4, 180)
	source.Write(gbOverwriteVideoHeader(0xb7, 200))
	source.Write(gbOverwriteVideo(avframe.FrameTypeKeyframe, 0xb5, 220))
	assertGBPSPack(t, h.capture.readPack(t), avframe.MediaTypeVideo, 0xb7, 200)
	assertGBPSPack(t, h.capture.readPack(t), avframe.MediaTypeVideo, 0xb5, 220)
	source.Write(gbOverwriteVideo(avframe.FrameTypeInterframe, 0xb6, 240))
	assertGBPSPack(t, h.capture.readPack(t), avframe.MediaTypeVideo, 0xb6, 240)

	if got := muxerCreations.Load(); got != 2 {
		t.Fatalf("PS muxer creations after source overwrite = %d, want 2", got)
	}
	if got := h.capture.sequenceNumbers(); !gbRTPSequencesConsecutive(got) {
		t.Fatalf("RTP sequence numbers are not monotonic across source recovery: %v", got)
	}
}

func TestGBOutboundTranscodedTargetAudioOverwriteKeepsDirectVideoAndPS(t *testing.T) {
	events, pauses := installGBOverwriteLogObserver(t, true)
	h := newGBDualOverwriteHarness(t, "gb28181/transcoded-target-audio-overwrite")
	source := util.NewRingBuffer[*avframe.AVFrame](16)
	audio := util.NewRingBuffer[*avframe.AVFrame](2)
	source.Write(gbOverwriteVideo(avframe.FrameTypeInterframe, 0xb0, 100))
	for _, marker := range []byte{0xa1, 0xa2, 0xa3, 0xa4} {
		audio.Write(gbOverwriteAudio(marker, int64(marker)))
	}

	var muxerCreations atomic.Int32
	sourceStarted := make(chan struct{})
	releaseSource := make(chan struct{})
	var releaseSourceOnce sync.Once
	t.Cleanup(func() { releaseSourceOnce.Do(func() { close(releaseSource) }) })
	h.start(source.NewReaderAt(0), audio.NewReaderAt(0), func(sender *outboundMediaSession) {
		sender.newPSMuxer = func() *ps.Muxer {
			muxerCreations.Add(1)
			return ps.NewMuxer()
		}
		sender.pumpObserver = &gbMediaPumpObserver{started: func(reader gbMediaReader) {
			if reader == gbMediaReaderSource {
				close(sourceStarted)
				<-releaseSource
			}
		}}
	})
	assertGBOverwriteEvent(t, events, gbOverwriteLogEvent{
		reader: "target_audio", action: "continue_audio", overwritten: 2,
	})
	waitGBSignal(t, sourceStarted, "source pump did not park before target-audio overwrite")
	releaseSourceOnce.Do(func() { close(releaseSource) })
	close(<-pauses)
	audio.Write(gbOverwriteAudio(0xb1, 120))
	first := h.capture.readPack(t)
	second := h.capture.readPack(t)
	assertGBPSPack(t, first, avframe.MediaTypeVideo, 0xb0, 100)
	assertGBPSPack(t, second, avframe.MediaTypeAudio, 0xb1, 120)
	source.Write(gbOverwriteVideo(avframe.FrameTypeInterframe, 0xb2, 140))
	assertGBPSPack(t, h.capture.readPack(t), avframe.MediaTypeVideo, 0xb2, 140)

	if got := muxerCreations.Load(); got != 1 {
		t.Fatalf("target-audio overwrite reset PS muxer %d times, want startup only", got)
	}
}

func TestGBOutboundTranscodedOverwritePurgesMatchingPendingHoldback(t *testing.T) {
	t.Run("source_video", func(t *testing.T) {
		events, pauses := installGBOverwriteLogObserver(t, true)
		h := newGBDualOverwriteHarness(t, "gb28181/pending-source-overwrite")
		source := util.NewRingBuffer[*avframe.AVFrame](2)
		audio := util.NewRingBuffer[*avframe.AVFrame](8)
		pending := make(chan struct{})
		releasePending := make(chan struct{})
		readerParked := make(chan struct{})
		releaseReader := make(chan struct{})
		overwriteRead := make(chan struct{})
		var sourceReads atomic.Int32
		var releasePendingOnce, releaseReaderOnce, overwriteReadOnce sync.Once
		t.Cleanup(func() {
			releasePendingOnce.Do(func() { close(releasePending) })
			releaseReaderOnce.Do(func() { close(releaseReader) })
		})
		h.start(source.NewReaderAt(0), audio.NewReaderAt(0), func(sender *outboundMediaSession) {
			sender.pumpObserver = &gbMediaPumpObserver{
				beforeRead: func(reader gbMediaReader) {
					if reader == gbMediaReaderSource && sourceReads.Add(1) == 2 {
						close(readerParked)
						<-releaseReader
					}
				},
				read: func(reader gbMediaReader, result util.RingReadResult[*avframe.AVFrame]) {
					if reader == gbMediaReaderSource && result.Overwritten > 0 {
						overwriteReadOnce.Do(func() { close(overwriteRead) })
					}
				},
				pending: func(reader gbMediaReader, frame *avframe.AVFrame) {
					if reader == gbMediaReaderSource && frame != nil && slices.Contains(frame.Payload, byte(0xa0)) {
						close(pending)
						<-releasePending
					}
				},
			}
		})
		source.Write(gbOverwriteVideoHeader(0xa0, 20))
		waitGBSignal(t, pending, "source frame did not enter pending holdback")
		waitGBSignal(t, readerParked, "source reader did not park before overwrite")
		for _, frame := range []*avframe.AVFrame{
			gbOverwriteVideo(avframe.FrameTypeInterframe, 0xa1, 40),
			gbOverwriteVideo(avframe.FrameTypeInterframe, 0xa2, 60),
			gbOverwriteVideo(avframe.FrameTypeInterframe, 0xa3, 80),
		} {
			source.Write(frame)
		}
		releaseReaderOnce.Do(func() { close(releaseReader) })
		releasePendingOnce.Do(func() { close(releasePending) })
		waitGBSignal(t, overwriteRead, "source reader did not observe overwrite")
		assertGBOverwriteEvent(t, events, gbOverwriteLogEvent{
			reader: "source", action: "wait_keyframe", overwritten: 1,
		})
		close(<-pauses)

		audio.Write(gbOverwriteAudio(0xb0, 100))
		assertGBPSPack(t, h.capture.readPack(t), avframe.MediaTypeAudio, 0xb0, 100)
		source.Write(gbOverwriteVideo(avframe.FrameTypeKeyframe, 0xb1, 120))
		audio.Write(gbOverwriteAudio(0xb2, 140))
		assertGBPSPack(t, h.capture.readPack(t), avframe.MediaTypeAudio, 0xb2, 140)
		source.Write(gbOverwriteVideoHeader(0xb3, 160))
		source.Write(gbOverwriteVideo(avframe.FrameTypeKeyframe, 0xb4, 180))
		assertGBPSPack(t, h.capture.readPack(t), avframe.MediaTypeVideo, 0xb3, 160)
		assertGBPSPack(t, h.capture.readPack(t), avframe.MediaTypeVideo, 0xb4, 180)
	})

	t.Run("target_audio", func(t *testing.T) {
		events, pauses := installGBOverwriteLogObserver(t, true)
		h := newGBDualOverwriteHarness(t, "gb28181/pending-target-overwrite")
		source := util.NewRingBuffer[*avframe.AVFrame](8)
		audio := util.NewRingBuffer[*avframe.AVFrame](2)
		pending := make(chan struct{})
		releasePending := make(chan struct{})
		readerParked := make(chan struct{})
		releaseReader := make(chan struct{})
		overwriteRead := make(chan struct{})
		var audioReads atomic.Int32
		var releasePendingOnce, releaseReaderOnce, overwriteReadOnce sync.Once
		t.Cleanup(func() {
			releasePendingOnce.Do(func() { close(releasePending) })
			releaseReaderOnce.Do(func() { close(releaseReader) })
		})
		h.start(source.NewReaderAt(0), audio.NewReaderAt(0), func(sender *outboundMediaSession) {
			sender.pumpObserver = &gbMediaPumpObserver{
				beforeRead: func(reader gbMediaReader) {
					if reader == gbMediaReaderTargetAudio && audioReads.Add(1) == 2 {
						close(readerParked)
						<-releaseReader
					}
				},
				read: func(reader gbMediaReader, result util.RingReadResult[*avframe.AVFrame]) {
					if reader == gbMediaReaderTargetAudio && result.Overwritten > 0 {
						overwriteReadOnce.Do(func() { close(overwriteRead) })
					}
				},
				pending: func(reader gbMediaReader, frame *avframe.AVFrame) {
					if reader == gbMediaReaderTargetAudio && frame != nil && slices.Contains(frame.Payload, byte(0xa0)) {
						close(pending)
						<-releasePending
					}
				},
			}
		})
		audio.Write(gbOverwriteAudio(0xa0, 20))
		waitGBSignal(t, pending, "target audio did not enter pending holdback")
		waitGBSignal(t, readerParked, "target-audio reader did not park before overwrite")
		for _, frame := range []*avframe.AVFrame{
			gbOverwriteAudio(0xa1, 40),
			gbOverwriteAudio(0xa2, 60),
			gbOverwriteAudio(0xa3, 80),
		} {
			audio.Write(frame)
		}
		releaseReaderOnce.Do(func() { close(releaseReader) })
		releasePendingOnce.Do(func() { close(releasePending) })
		waitGBSignal(t, overwriteRead, "target-audio reader did not observe overwrite")
		assertGBOverwriteEvent(t, events, gbOverwriteLogEvent{
			reader: "target_audio", action: "continue_audio", overwritten: 1,
		})
		close(<-pauses)

		source.Write(gbOverwriteVideo(avframe.FrameTypeInterframe, 0xb0, 100))
		source.Write(gbOverwriteVideo(avframe.FrameTypeInterframe, 0xb1, 120))
		assertGBPSPack(t, h.capture.readPack(t), avframe.MediaTypeVideo, 0xb0, 100)
	})
}

func TestGBOutboundTranscodedControlPrecedesBothPendingMedia(t *testing.T) {
	t.Run("source_overwrite", func(t *testing.T) {
		events, pauses := installGBOverwriteLogObserver(t, true)
		h := newGBDualOverwriteHarness(t, "gb28181/both-pending-source-overwrite")
		source := util.NewRingBuffer[*avframe.AVFrame](2)
		audio := util.NewRingBuffer[*avframe.AVFrame](8)
		probe := newGBBothPendingControlProbe(t, gbMediaReaderSource)
		h.start(source.NewReaderAt(0), audio.NewReaderAt(0), func(sender *outboundMediaSession) {
			sender.pumpObserver = probe.observer()
		})

		source.Write(gbOverwriteVideoHeader(0xa0, 20))
		probe.waitFirstPending(t)
		probe.waitReaderParked(t)
		audio.Write(gbOverwriteAudio(0xa1, 40))
		probe.waitBothPending(t)
		for _, frame := range []*avframe.AVFrame{
			gbOverwriteVideo(avframe.FrameTypeInterframe, 0xb0, 60),
			gbOverwriteVideo(avframe.FrameTypeInterframe, 0xb1, 80),
			gbOverwriteVideo(avframe.FrameTypeInterframe, 0xb2, 100),
		} {
			source.Write(frame)
		}
		probe.releaseReader()
		probe.waitControlQueued(t)
		probe.releasePending()
		assertGBOverwriteEvent(t, events, gbOverwriteLogEvent{
			reader: "source", action: "wait_keyframe", overwritten: 1,
		})
		pause := <-pauses
		defer close(pause)
		assertNoGBRTPPacket(t, h.capture.conn)
	})

	t.Run("target_audio_overwrite", func(t *testing.T) {
		events, pauses := installGBOverwriteLogObserver(t, true)
		h := newGBDualOverwriteHarness(t, "gb28181/both-pending-target-overwrite")
		source := util.NewRingBuffer[*avframe.AVFrame](8)
		audio := util.NewRingBuffer[*avframe.AVFrame](2)
		probe := newGBBothPendingControlProbe(t, gbMediaReaderTargetAudio)
		h.start(source.NewReaderAt(0), audio.NewReaderAt(0), func(sender *outboundMediaSession) {
			sender.pumpObserver = probe.observer()
		})

		audio.Write(gbOverwriteAudio(0xa0, 20))
		probe.waitFirstPending(t)
		probe.waitReaderParked(t)
		source.Write(gbOverwriteVideo(avframe.FrameTypeInterframe, 0xa1, 40))
		probe.waitBothPending(t)
		for _, frame := range []*avframe.AVFrame{
			gbOverwriteAudio(0xb0, 60),
			gbOverwriteAudio(0xb1, 80),
			gbOverwriteAudio(0xb2, 100),
		} {
			audio.Write(frame)
		}
		probe.releaseReader()
		probe.waitControlQueued(t)
		probe.releasePending()
		assertGBOverwriteEvent(t, events, gbOverwriteLogEvent{
			reader: "target_audio", action: "continue_audio", overwritten: 1,
		})
		pause := <-pauses
		defer close(pause)
		assertNoGBRTPPacket(t, h.capture.conn)
	})

	t.Run("target_audio_eof", func(t *testing.T) {
		h := newGBDualOverwriteHarness(t, "gb28181/both-pending-target-eof")
		source := util.NewRingBuffer[*avframe.AVFrame](8)
		audio := util.NewRingBuffer[*avframe.AVFrame](8)
		probe := newGBBothPendingControlProbe(t, gbMediaReaderTargetAudio)
		h.start(source.NewReaderAt(0), audio.NewReaderAt(0), func(sender *outboundMediaSession) {
			sender.pumpObserver = probe.observer()
		})

		audio.Write(gbOverwriteAudio(0xa0, 20))
		probe.waitFirstPending(t)
		probe.waitReaderParked(t)
		source.Write(gbOverwriteVideo(avframe.FrameTypeInterframe, 0xa1, 40))
		probe.waitBothPending(t)
		audio.Close()
		probe.releaseReader()
		probe.waitControlQueued(t)
		probe.releasePending()
		err := h.wait()
		if err == nil || !strings.Contains(err.Error(), "target audio ended") {
			t.Fatalf("active target-audio EOF error = %v, want explicit failure", err)
		}
		assertNoGBRTPPacket(t, h.capture.conn)
	})
}

func TestGBOutboundTranscodedTargetOverwritePreservesPendingVideoHoldbackDeadline(t *testing.T) {
	events, pauses := installGBOverwriteLogObserver(t, true)
	h := newGBDualOverwriteHarness(t, "gb28181/target-overwrite-holdback-deadline")
	source := util.NewRingBuffer[*avframe.AVFrame](8)
	audio := util.NewRingBuffer[*avframe.AVFrame](2)
	probe := newGBBothPendingControlProbe(t, gbMediaReaderTargetAudio)
	holdbackStarted := make(chan time.Time, 2)
	holdbackFired := make(chan time.Time, 1)
	h.start(source.NewReaderAt(0), audio.NewReaderAt(0), func(sender *outboundMediaSession) {
		observer := probe.observer()
		observer.holdbackStarted = func(deadline time.Time) { holdbackStarted <- deadline }
		observer.holdbackFired = func(deadline time.Time) { holdbackFired <- deadline }
		sender.pumpObserver = observer
	})

	audio.Write(gbOverwriteAudio(0xa0, 20))
	probe.waitFirstPending(t)
	probe.waitReaderParked(t)
	var originalDeadline time.Time
	select {
	case originalDeadline = <-holdbackStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("initial target-audio holdback deadline was not observed")
	}
	source.Write(gbOverwriteVideo(avframe.FrameTypeInterframe, 0xa1, 40))
	probe.waitBothPending(t)
	for _, frame := range []*avframe.AVFrame{
		gbOverwriteAudio(0xb0, 60),
		gbOverwriteAudio(0xb1, 80),
		gbOverwriteAudio(0xb2, 100),
	} {
		audio.Write(frame)
	}
	probe.releaseReader()
	probe.waitControlQueued(t)
	probe.releasePending()
	assertGBOverwriteEvent(t, events, gbOverwriteLogEvent{
		reader: "target_audio", action: "continue_audio", overwritten: 1,
	})
	close(<-pauses)

	select {
	case resetDeadline := <-holdbackStarted:
		t.Fatalf("target-audio overwrite reset pending-video holdback deadline from %s to %s", originalDeadline.Format(time.RFC3339Nano), resetDeadline.Format(time.RFC3339Nano))
	case firedDeadline := <-holdbackFired:
		if !firedDeadline.Equal(originalDeadline) {
			t.Fatalf("pending-video holdback fired deadline = %s, want original %s", firedDeadline.Format(time.RFC3339Nano), originalDeadline.Format(time.RFC3339Nano))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pending-video holdback neither fired nor reported a reset")
	}
	assertGBPSPack(t, h.capture.readPack(t), avframe.MediaTypeVideo, 0xa1, 40)
}

func TestGBOutboundTranscodedActiveTargetAudioEOFJoinsPumps(t *testing.T) {
	h := newGBDualOverwriteHarness(t, "gb28181/active-target-eof")
	source := util.NewRingBuffer[*avframe.AVFrame](4)
	audio := util.NewRingBuffer[*avframe.AVFrame](4)
	probe := newGBPumpJoinProbe()
	h.start(source.NewReaderAt(0), audio.NewReaderAt(0), func(sender *outboundMediaSession) {
		sender.pumpObserver = probe.observer()
	})
	probe.waitStarted(t)

	source.Write(gbOverwriteVideo(avframe.FrameTypeInterframe, 0xd0, 100))
	audio.Write(gbOverwriteAudio(0xd1, 120))
	assertGBPSPack(t, h.capture.readPack(t), avframe.MediaTypeVideo, 0xd0, 100)
	assertGBPSPack(t, h.capture.readPack(t), avframe.MediaTypeAudio, 0xd1, 120)
	audio.Close()

	err := h.wait()
	if err == nil || !strings.Contains(err.Error(), "target audio ended") {
		t.Fatalf("active target-audio EOF error = %v, want explicit failure", err)
	}
	probe.assertJoined(t)
	source.Write(gbOverwriteVideo(avframe.FrameTypeInterframe, 0xee, 140))
	assertNoGBRTPPacket(t, h.capture.conn)
}

func TestGBOutboundTranscodedReplacementAndCancellationJoinPumps(t *testing.T) {
	t.Run("publisher_replacement", func(t *testing.T) {
		h := newGBDualOverwriteHarness(t, "gb28181/generation-replacement")
		audio := util.NewRingBuffer[*avframe.AVFrame](4)
		probe := newGBPumpJoinProbe()
		h.start(h.stream.RingBuffer().NewReaderAt(h.sender.snapshot.LiveCursor), audio.NewReaderAt(0), func(sender *outboundMediaSession) {
			sender.pumpObserver = probe.observer()
		})
		probe.waitStarted(t)
		h.stream.WriteFrame(gbOverwriteVideo(avframe.FrameTypeInterframe, 0xd0, 100))
		audio.Write(gbOverwriteAudio(0xd1, 120))
		assertGBPSPack(t, h.capture.readPack(t), avframe.MediaTypeVideo, 0xd0, 100)
		assertGBPSPack(t, h.capture.readPack(t), avframe.MediaTypeAudio, 0xd1, 120)

		h.stream.RemovePublisher()
		if err := h.stream.SetPublisher(&gbOutboundTestPublisher{id: "replacement", info: &avframe.MediaInfo{
			VideoCodec: avframe.CodecH264,
			AudioCodec: avframe.CodecOpus,
		}}); err != nil {
			t.Fatalf("SetPublisher replacement: %v", err)
		}
		h.stream.WriteFrame(gbOverwriteVideo(avframe.FrameTypeInterframe, 0xe0, 140))
		audio.Write(gbOverwriteAudio(0xe1, 160))
		if err := h.wait(); err != nil {
			t.Fatalf("generation replacement returned error: %v", err)
		}
		probe.assertJoined(t)
		assertNoGBRTPPacket(t, h.capture.conn)
	})

	t.Run("explicit_cancellation", func(t *testing.T) {
		h := newGBDualOverwriteHarness(t, "gb28181/explicit-cancellation")
		source := util.NewRingBuffer[*avframe.AVFrame](4)
		audio := util.NewRingBuffer[*avframe.AVFrame](4)
		probe := newGBPumpJoinProbe()
		h.start(source.NewReaderAt(0), audio.NewReaderAt(0), func(sender *outboundMediaSession) {
			sender.pumpObserver = probe.observer()
		})
		probe.waitStarted(t)
		h.cancel()
		if err := h.wait(); err != nil {
			t.Fatalf("explicit cancellation returned error: %v", err)
		}
		probe.assertJoined(t)
	})
}

func TestGBOutboundSharedTranscodeProducerSourceOverwriteFailsAndReleasesOnce(t *testing.T) {
	stream := newGBOverwriteStream(t, "gb28181/transcode-producer-overwrite", 2, avframe.CodecG711U)
	snapshot := stream.StartupSnapshot()
	for _, frame := range []*avframe.AVFrame{
		gbOverwriteAudioCodec(avframe.CodecG711U, 0xa1, 0),
		gbOverwriteAudioCodec(avframe.CodecG711U, 0xa2, 20),
		gbOverwriteAudioCodec(avframe.CodecG711U, 0xa3, 40),
		gbOverwriteAudioCodec(avframe.CodecG711U, 0xa4, 60),
	} {
		stream.WriteFrame(frame)
	}
	manager := core.NewTranscodeManager(stream, audiocodec.Global(), 8)
	core.SetTranscodeManagerForTest(stream, manager)
	targetReader, releaseTarget, err := manager.GetOrCreateAudioReaderAtFromHistory(avframe.CodecG711A, snapshot)
	if err != nil {
		t.Fatalf("GetOrCreateAudioReaderAtFromHistory: %v", err)
	}

	sender, err := newOutboundMediaSession(stream, 0, 0)
	if err != nil {
		t.Fatalf("newOutboundMediaSession: %v", err)
	}
	sender.snapshot = snapshot
	remoteRTP, remoteRTCP, err := listenGBLabUDPPair()
	if err != nil {
		sender.close()
		t.Fatalf("listen remote media pair: %v", err)
	}
	defer remoteRTP.Close()
	defer remoteRTCP.Close()
	if err := sender.setRemote(remoteRTP.LocalAddr().(*net.UDPAddr)); err != nil {
		sender.close()
		t.Fatalf("setRemote: %v", err)
	}
	sender.audio = targetReader
	var targetReleases atomic.Int32
	sender.releaseAudio = func() {
		targetReleases.Add(1)
		releaseTarget()
	}
	if err := sender.admit(); err != nil {
		sender.close()
		t.Fatalf("admit: %v", err)
	}
	sender.start()

	select {
	case err := <-sender.done:
		if err == nil || !strings.Contains(err.Error(), "target audio ended") {
			t.Fatalf("producer source-overwrite error = %v, want target-audio failure", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("producer source overwrite did not fail the GB28181 session")
	}
	assertNoGBRTPPacket(t, remoteRTP)
	sender.close()
	sender.close()
	if got := targetReleases.Load(); got != 1 {
		t.Fatalf("target-audio releases = %d, want 1", got)
	}
	if got := stream.Subscribers()["gb28181"]; got != 0 {
		t.Fatalf("GB28181 subscribers after producer overwrite = %d, want 0", got)
	}
}

type gbBothPendingControlProbe struct {
	t             *testing.T
	target        gbMediaReader
	firstPending  chan struct{}
	bothPending   chan struct{}
	releaseMerge  chan struct{}
	readerParked  chan struct{}
	releaseRead   chan struct{}
	controlQueued chan struct{}
	pendingCount  atomic.Int32
	readCount     atomic.Int32
	queueCount    atomic.Int32
	releaseOnce   sync.Once
	readOnce      sync.Once
}

func newGBBothPendingControlProbe(t *testing.T, target gbMediaReader) *gbBothPendingControlProbe {
	t.Helper()
	p := &gbBothPendingControlProbe{
		t:             t,
		target:        target,
		firstPending:  make(chan struct{}),
		bothPending:   make(chan struct{}),
		releaseMerge:  make(chan struct{}),
		readerParked:  make(chan struct{}),
		releaseRead:   make(chan struct{}),
		controlQueued: make(chan struct{}),
	}
	t.Cleanup(func() {
		p.releasePending()
		p.releaseReader()
	})
	return p
}

func (p *gbBothPendingControlProbe) observer() *gbMediaPumpObserver {
	return &gbMediaPumpObserver{
		beforeRead: func(reader gbMediaReader) {
			if reader == p.target && p.readCount.Add(1) == 2 {
				close(p.readerParked)
				<-p.releaseRead
			}
		},
		queued: func(reader gbMediaReader) {
			if reader == p.target && p.queueCount.Add(1) == 2 {
				close(p.controlQueued)
			}
		},
		pending: func(_ gbMediaReader, _ *avframe.AVFrame) {
			switch p.pendingCount.Add(1) {
			case 1:
				close(p.firstPending)
			case 2:
				close(p.bothPending)
				<-p.releaseMerge
			}
		},
	}
}

func (p *gbBothPendingControlProbe) waitFirstPending(t *testing.T) {
	waitGBSignal(t, p.firstPending, "first affected frame did not become pending")
}

func (p *gbBothPendingControlProbe) waitBothPending(t *testing.T) {
	waitGBSignal(t, p.bothPending, "source video and target audio did not both become pending")
}

func (p *gbBothPendingControlProbe) waitReaderParked(t *testing.T) {
	waitGBSignal(t, p.readerParked, "affected reader did not park before the control result")
}

func (p *gbBothPendingControlProbe) waitControlQueued(t *testing.T) {
	waitGBSignal(t, p.controlQueued, "affected reader did not queue the control result")
}

func (p *gbBothPendingControlProbe) releasePending() {
	p.releaseOnce.Do(func() { close(p.releaseMerge) })
}

func (p *gbBothPendingControlProbe) releaseReader() {
	p.readOnce.Do(func() { close(p.releaseRead) })
}

type gbPumpJoinProbe struct {
	started chan gbMediaReader
	joined  chan uint32
	exited  atomic.Uint32
}

func newGBPumpJoinProbe() *gbPumpJoinProbe {
	return &gbPumpJoinProbe{
		started: make(chan gbMediaReader, 2),
		joined:  make(chan uint32, 1),
	}
}

func (p *gbPumpJoinProbe) observer() *gbMediaPumpObserver {
	return &gbMediaPumpObserver{
		started: func(reader gbMediaReader) { p.started <- reader },
		exited: func(reader gbMediaReader) {
			switch reader {
			case gbMediaReaderSource:
				p.exited.Or(1)
			case gbMediaReaderTargetAudio:
				p.exited.Or(2)
			}
		},
		joined: func() { p.joined <- p.exited.Load() },
	}
}

func (p *gbPumpJoinProbe) waitStarted(t *testing.T) {
	t.Helper()
	seen := make(map[gbMediaReader]bool, 2)
	for len(seen) < 2 {
		select {
		case reader := <-p.started:
			seen[reader] = true
		case <-time.After(2 * time.Second):
			t.Fatal("GB28181 media pumps did not both start")
		}
	}
}

func (p *gbPumpJoinProbe) assertJoined(t *testing.T) {
	t.Helper()
	select {
	case exited := <-p.joined:
		if exited != 3 {
			t.Fatalf("pump exits at joined boundary = %02b, want 11", exited)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("GB28181 media parent did not join both pumps")
	}
}

type gbDualOverwriteHarness struct {
	t       *testing.T
	stream  *core.Stream
	sender  *outboundMediaSession
	capture *gbPSCapture
	cancel  context.CancelFunc
	done    chan error
	once    sync.Once
	started bool
	result  error
}

func newGBDualOverwriteHarness(t *testing.T, key string) *gbDualOverwriteHarness {
	t.Helper()
	stream := newGBOverwriteStream(t, key, 16, avframe.CodecOpus)
	sender, err := newOutboundMediaSession(stream, 0, 0)
	if err != nil {
		t.Fatalf("newOutboundMediaSession: %v", err)
	}
	sender.snapshot = stream.StartupSnapshot()
	remoteRTP, remoteRTCP, err := listenGBLabUDPPair()
	if err != nil {
		sender.close()
		t.Fatalf("listen remote media pair: %v", err)
	}
	if err := sender.setRemote(remoteRTP.LocalAddr().(*net.UDPAddr)); err != nil {
		remoteRTP.Close()
		remoteRTCP.Close()
		sender.close()
		t.Fatalf("setRemote: %v", err)
	}
	h := &gbDualOverwriteHarness{
		t:       t,
		stream:  stream,
		sender:  sender,
		capture: &gbPSCapture{conn: remoteRTP, demuxer: ps.NewDemuxer()},
		done:    make(chan error, 1),
	}
	t.Cleanup(func() {
		h.stop()
		_ = remoteRTP.Close()
		_ = remoteRTCP.Close()
		_ = sender.rtpConn.Close()
		_ = sender.rtcpConn.Close()
	})
	return h
}

func (h *gbDualOverwriteHarness) start(
	sourceReader, audioReader *util.RingReader[*avframe.AVFrame],
	configure func(*outboundMediaSession),
) {
	if configure != nil {
		configure(h.sender)
	}
	ctx, cancel := bindGBGeneration(context.Background(), h.sender.snapshot)
	h.cancel = cancel
	h.started = true
	go func() {
		h.done <- h.sender.runTranscodedMedia(ctx, h.sender.freshPSMuxer(), nil, sourceReader, audioReader)
	}()
}

func waitGBSignal(t *testing.T, signal <-chan struct{}, failure string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatal(failure)
	}
}

func (h *gbDualOverwriteHarness) stop() error {
	return h.finish(true)
}

func (h *gbDualOverwriteHarness) wait() error {
	return h.finish(false)
}

func (h *gbDualOverwriteHarness) finish(cancel bool) error {
	h.once.Do(func() {
		if !h.started {
			return
		}
		if cancel && h.cancel != nil {
			h.cancel()
		}
		select {
		case h.result = <-h.done:
		case <-time.After(2 * time.Second):
			h.t.Error("GB28181 dual media pumps did not stop within the deterministic bound")
		}
	})
	return h.result
}

func gbOverwriteAudioCodec(codec avframe.CodecType, marker byte, dts int64) *avframe.AVFrame {
	return avframe.NewAVFrame(
		avframe.MediaTypeAudio, codec, avframe.FrameTypeInterframe,
		dts, dts, []byte{marker},
	)
}

func assertNoGBRTPPacket(t *testing.T, conn *net.UDPConn) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	buf := make([]byte, 2048)
	if _, _, err := conn.ReadFromUDP(buf); err == nil {
		t.Fatal("unexpected GB28181 RTP packet after terminal boundary")
	} else if netErr, ok := err.(net.Error); !ok || !netErr.Timeout() {
		t.Fatalf("ReadFromUDP: %v", err)
	}
}
