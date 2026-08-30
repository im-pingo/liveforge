package webrtc

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/util"
)

func receiveWHEPReaderEvent(t *testing.T, events <-chan whepReaderEvent) whepReaderEvent {
	t.Helper()
	select {
	case event, ok := <-events:
		if !ok {
			t.Fatal("WHEP reader event stream closed before an event")
		}
		acknowledgeWHEPReaderEvent(&event)
		return event
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for WHEP reader event")
		return whepReaderEvent{}
	}
}

func TestWHEPReaderPumpEmitsOneAtomicOverwriteEvent(t *testing.T) {
	ring := util.NewRingBuffer[*avframe.AVFrame](2)
	reader := ring.NewReaderAt(0)
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan whepReaderEvent, 1)
	var terminal atomic.Uint32

	first := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecVP8, avframe.FrameTypeKeyframe, 0, 0, []byte{0xa0})
	retained := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecVP8, avframe.FrameTypeInterframe, 20, 20, []byte{0xa1})
	live := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecVP8, avframe.FrameTypeInterframe, 40, 40, []byte{0xa2})
	ring.Write(first)
	ring.Write(retained)
	ring.Write(live)
	go pumpWHEPReader(ctx, whepReaderSource, reader, events, &terminal)

	event := receiveWHEPReaderEvent(t, events)
	if event.reader != whepReaderSource {
		t.Fatalf("overwrite event reader = %q, want source", event.reader)
	}
	if event.terminal != whepReaderTerminalNone {
		t.Fatalf("overwrite event terminal cause = %q, want none", event.terminal)
	}
	if event.result.Value != retained || event.result.Overwritten != 1 || !event.result.OK {
		t.Fatalf("atomic overwrite result = %+v, want retained frame with one overwrite", event.result)
	}
	if got := reader.ReadCursor(); got != ring.WriteCursor() {
		t.Fatalf("source cursor after overwrite event = %d, want live cursor %d", got, ring.WriteCursor())
	}

	cancel()
	terminalEvent := receiveWHEPReaderEvent(t, events)
	if terminalEvent.terminal != whepReaderTerminalCanceled {
		t.Fatalf("canceled pump cause = %q, want canceled", terminalEvent.terminal)
	}
}

func TestWHEPReaderPumpDistinguishesEOFFromGenerationEnd(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*util.RingBuffer[*avframe.AVFrame], context.CancelCauseFunc)
		want  whepReaderTerminalCause
	}{
		{
			name: "ring eof",
			setup: func(ring *util.RingBuffer[*avframe.AVFrame], _ context.CancelCauseFunc) {
				ring.Close()
			},
			want: whepReaderTerminalEOF,
		},
		{
			name: "generation end",
			setup: func(_ *util.RingBuffer[*avframe.AVFrame], cancel context.CancelCauseFunc) {
				cancel(errWHEPReaderGenerationEnded)
			},
			want: whepReaderTerminalGenerationEnded,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ring := util.NewRingBuffer[*avframe.AVFrame](1)
			ctx, cancel := context.WithCancelCause(context.Background())
			events := make(chan whepReaderEvent, 1)
			var terminal atomic.Uint32
			go pumpWHEPReader(ctx, whepReaderTargetAudio, ring.NewReaderAt(0), events, &terminal)
			test.setup(ring, cancel)

			event := receiveWHEPReaderEvent(t, events)
			if event.reader != whepReaderTargetAudio {
				t.Fatalf("terminal event reader = %q, want target audio", event.reader)
			}
			if event.terminal != test.want {
				t.Fatalf("terminal cause = %q, want %q", event.terminal, test.want)
			}
			if event.result.OK {
				t.Fatalf("terminal event retained a media result: %+v", event.result)
			}
			if test.want == whepReaderTerminalGenerationEnded && !errors.Is(context.Cause(ctx), errWHEPReaderGenerationEnded) {
				t.Fatalf("context cause = %v, want generation end", context.Cause(ctx))
			}
		})
	}
}

func TestWHEPFeedReadersPreserveIndependentReaderIdentity(t *testing.T) {
	sourceRing := util.NewRingBuffer[*avframe.AVFrame](2)
	targetRing := util.NewRingBuffer[*avframe.AVFrame](2)
	sourceReader := sourceRing.NewReaderAt(0)
	targetReader := targetRing.NewReaderAt(0)
	readers := &whepFeedReaders{source: sourceReader, targetAudio: targetReader}
	done := make(chan struct{})
	generationDone := make(chan struct{})
	readers.startWaiters(done, generationDone)
	t.Cleanup(readers.Close)

	sourceFrame := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecVP8, avframe.FrameTypeKeyframe, 0, 0, []byte{0xb0})
	targetFrame := avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecOpus, avframe.FrameTypeInterframe, 0, 0, []byte{0xc0})
	sourceRing.Write(sourceFrame)
	targetRing.Write(targetFrame)

	var sourceEvent, targetEvent whepReaderEvent
	var sourceOK, targetOK bool
	for !sourceOK || !targetOK {
		if !readers.wait(done, generationDone) {
			t.Fatal("reader wait stopped before both independent events arrived")
		}
		if event, ok := readers.tryReadSource(); ok {
			sourceEvent, sourceOK = event, true
		}
		if event, ok := readers.tryReadTargetAudio(); ok {
			targetEvent, targetOK = event, true
		}
	}

	if sourceEvent.reader != whepReaderSource || sourceEvent.result.Value != sourceFrame {
		t.Fatalf("source event = %+v, want source frame identity", sourceEvent)
	}
	if targetEvent.reader != whepReaderTargetAudio || targetEvent.result.Value != targetFrame {
		t.Fatalf("target-audio event = %+v, want target frame identity", targetEvent)
	}
	if got := sourceReader.ReadCursor(); got != 1 {
		t.Fatalf("source reader cursor = %d, want 1", got)
	}
	if got := targetReader.ReadCursor(); got != 1 {
		t.Fatalf("target-audio reader cursor = %d, want 1", got)
	}
}

func TestWHEPFeedReadersCloseJoinsPumpsAndReleasesOnce(t *testing.T) {
	sourceRing := util.NewRingBuffer[*avframe.AVFrame](1)
	targetRing := util.NewRingBuffer[*avframe.AVFrame](1)
	var releases int
	readers := &whepFeedReaders{
		source:      sourceRing.NewReaderAt(0),
		targetAudio: targetRing.NewReaderAt(0),
		release:     func() { releases++ },
	}
	readers.startWaiters(make(chan struct{}), make(chan struct{}))

	readers.Close()
	readers.Close()
	if releases != 1 {
		t.Fatalf("target-audio release calls = %d, want 1", releases)
	}
	for _, events := range map[string]<-chan whepReaderEvent{
		"source":       readers.sourceEvents,
		"target_audio": readers.audioEvents,
	} {
		for {
			_, ok := <-events
			if !ok {
				break
			}
		}
	}
}

func TestWHEPFeedReadersDoNotClassifyCancellationOrGenerationEndAsAudioEOF(t *testing.T) {
	for _, test := range []struct {
		name string
		stop func(chan struct{}, chan struct{})
	}{
		{
			name: "session cancellation",
			stop: func(done, _ chan struct{}) { close(done) },
		},
		{
			name: "generation end",
			stop: func(_, generationDone chan struct{}) { close(generationDone) },
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			readers := &whepFeedReaders{
				source:      util.NewRingBuffer[*avframe.AVFrame](1).NewReaderAt(0),
				targetAudio: util.NewRingBuffer[*avframe.AVFrame](1).NewReaderAt(0),
			}
			done := make(chan struct{})
			generationDone := make(chan struct{})
			readers.startWaiters(done, generationDone)
			test.stop(done, generationDone)
			readers.waitGroup.Wait()
			if readers.activeTargetAudioEOF(done, generationDone) {
				t.Fatal("lifecycle terminal cause was classified as target-audio EOF")
			}
			readers.Close()
		})
	}
}

func TestWHEPFeedReadersCloseUnblocksPermitAfterReady(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	readers := &whepFeedReaders{waitContext: ctx}
	permit := make(chan struct{})
	done := make(chan bool, 1)
	go func() {
		done <- readers.grantRead(permit, nil, nil)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case ok := <-done:
		if ok {
			t.Fatal("permit delivery succeeded after lifecycle cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("permit delivery remained blocked after lifecycle cancellation")
	}
}

func TestWHEPFeedReaderFastPathPermitObservesLifecycleCancellation(t *testing.T) {
	for _, test := range []struct {
		name  string
		ready func(*whepFeedReaders) chan struct{}
		try   func(*whepFeedReaders) (whepReaderEvent, bool)
	}{
		{
			name: "source",
			ready: func(readers *whepFeedReaders) chan struct{} {
				return readers.sourceReady
			},
			try: func(readers *whepFeedReaders) (whepReaderEvent, bool) {
				return readers.tryReadSource()
			},
		},
		{
			name: "target audio",
			ready: func(readers *whepFeedReaders) chan struct{} {
				return readers.audioReady
			},
			try: func(readers *whepFeedReaders) (whepReaderEvent, bool) {
				return readers.tryReadTargetAudio()
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			done := make(chan struct{})
			readers := &whepFeedReaders{
				closed:         true,
				waitContext:    context.Background(),
				done:           done,
				generationDone: make(chan struct{}),
			}
			readers.sourceReady = make(chan struct{})
			readers.sourcePermit = make(chan struct{})
			readers.sourceEvents = make(chan whepReaderEvent)
			readers.audioReady = make(chan struct{})
			readers.audioPermit = make(chan struct{})
			readers.audioEvents = make(chan whepReaderEvent)
			close(test.ready(readers))

			result := make(chan bool, 1)
			go func() {
				_, ok := test.try(readers)
				result <- ok
			}()
			close(done)
			select {
			case ok := <-result:
				if ok {
					t.Fatal("fast-path read succeeded after lifecycle cancellation")
				}
			case <-time.After(time.Second):
				t.Fatal("fast-path permit remained blocked after lifecycle cancellation")
			}
		})
	}
}

func TestWHEPReaderPumpPrefersGenerationEndWhenRingAlsoEOF(t *testing.T) {
	ring := util.NewRingBuffer[*avframe.AVFrame](1)
	reader := ring.NewReaderAt(0)
	ring.Close()
	generationDone := make(chan struct{})
	close(generationDone)
	events := make(chan whepReaderEvent, 1)
	var terminal atomic.Uint32
	ctx := context.Background()

	pumpWHEPReaderGated(ctx, whepReaderTargetAudio, reader, events, &terminal, nil, nil, nil, generationDone)
	event := receiveWHEPReaderEvent(t, events)
	if event.terminal != whepReaderTerminalGenerationEnded {
		t.Fatalf("terminal cause = %q, want generation end", event.terminal)
	}
}
