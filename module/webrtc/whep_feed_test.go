package webrtc

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/audiocodec"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/util"
	"github.com/pion/webrtc/v4"
)

func TestWHEPStartupSnapshotKeepsFramesWrittenWhileCacheIsSent(t *testing.T) {
	stream := core.NewStream("live/whep-snapshot", config.StreamConfig{
		GOPCache:       true,
		GOPCacheNum:    1,
		RingBufferSize: 16,
	}, config.LimitsConfig{}, core.NewEventBus())
	cached := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH265, avframe.FrameTypeKeyframe, 1000, 1000, []byte{1})
	stream.WriteFrame(cached)

	snapshot := stream.StartupSnapshot()
	gopCache := whepLiveSnapshot(snapshot, false)
	if len(gopCache) != 1 || gopCache[0] != cached {
		t.Fatalf("GOP snapshot = %v, want cached keyframe", gopCache)
	}

	live := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH265, avframe.FrameTypeInterframe, 1033, 1033, []byte{2})
	stream.WriteFrame(live) // Simulates a publisher write while the cached GOP is sent.
	got, ok := stream.RingBuffer().NewReaderAt(snapshot.LiveCursor).TryRead()
	if !ok || got != live {
		t.Fatalf("first live frame after GOP snapshot = (%v, %v), want newly written frame", got, ok)
	}
}

func TestWHEPStartupSnapshotDropsSourceAudioWhenTranscoding(t *testing.T) {
	stream := core.NewStream("live/whep-transcode-cache", config.StreamConfig{
		GOPCache:       true,
		GOPCacheNum:    1,
		RingBufferSize: 16,
	}, config.LimitsConfig{}, core.NewEventBus())
	video := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH265, avframe.FrameTypeKeyframe, 1000, 1000, []byte{1})
	aac := avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe, 1020, 1020, []byte{2})
	interframe := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH265, avframe.FrameTypeInterframe, 1040, 1040, []byte{3})
	stream.WriteFrame(video)
	stream.WriteFrame(aac)
	stream.WriteFrame(interframe)

	snapshot := stream.StartupSnapshot()
	alias := snapshot.ReplayFrames
	gopCache := whepLiveSnapshot(snapshot, true)
	if len(gopCache) != 2 || gopCache[0] != video || gopCache[1] != interframe {
		t.Fatalf("transcoded live GOP snapshot = %v, want cached video frames", gopCache)
	}
	if len(snapshot.ReplayFrames) != 3 || snapshot.ReplayFrames[0] != video || snapshot.ReplayFrames[1] != aac || snapshot.ReplayFrames[2] != interframe {
		t.Fatalf("startup snapshot was mutated while filtering: %v", snapshot.ReplayFrames)
	}
	if len(alias) != 3 || alias[0] != video || alias[1] != aac || alias[2] != interframe {
		t.Fatalf("startup snapshot alias was mutated while filtering: %v", alias)
	}
	gopCache[0] = nil
	if snapshot.ReplayFrames[0] != video || alias[0] != video {
		t.Fatal("filtered replay slice aliases the startup snapshot backing storage")
	}
}

func TestWHEPFeedReadersKeepAtomicSourceCursorWhenTranscoderUnavailable(t *testing.T) {
	stream := core.NewStream("live/whep-reader-transition", config.StreamConfig{
		RingBufferSize: 16,
	}, config.LimitsConfig{}, core.NewEventBus())
	if err := stream.SetPublisher(&authorizationTestPublisher{
		id:   "aac-publisher",
		info: &avframe.MediaInfo{VideoCodec: avframe.CodecH265, AudioCodec: avframe.CodecAAC},
	}); err != nil {
		t.Fatal(err)
	}
	core.SetTranscodeManagerForTest(stream, core.NewTranscodeManager(stream, &audiocodec.Registry{}, 16))

	snapshot := stream.StartupSnapshot()
	betweenSnapshotAndReader := avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH265, avframe.FrameTypeInterframe,
		1033, 1033, []byte{1},
	)
	stream.WriteFrame(betweenSnapshotAndReader)

	readers, err := newWHEPFeedReaders(stream, snapshot, true, avframe.CodecOpus)
	if err != nil {
		t.Fatalf("newWHEPFeedReaders: %v", err)
	}
	done := make(chan struct{})
	readers.startWaiters(done, snapshot.GenerationDone)
	defer func() {
		close(done)
		readers.Close()
	}()
	if !readers.wait(done, snapshot.GenerationDone) {
		t.Fatal("source reader wait stopped before the frame written after the snapshot")
	}
	read, ok := readers.tryReadSource()
	if !ok || read.result.Value != betweenSnapshotAndReader {
		t.Fatalf("source reader first frame = (%v, %v), want frame written after snapshot", read.result.Value, ok)
	}
	if readers.targetAudio == nil {
		t.Fatal("transcode reader missing")
	}

	if _, targetAudioOK := readers.tryReadTargetAudio(); targetAudioOK {
		t.Fatal("unavailable transcoder unexpectedly produced a frame")
	}

	woke := make(chan bool, 1)
	go func() {
		woke <- readers.wait(done, snapshot.GenerationDone)
	}()
	select {
	case <-woke:
		t.Fatal("reader wait returned before source video arrived")
	case <-time.After(20 * time.Millisecond):
	}

	afterUnavailableEpoch := avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH265, avframe.FrameTypeInterframe,
		1066, 1066, []byte{2},
	)
	stream.WriteFrame(afterUnavailableEpoch)
	select {
	case ok := <-woke:
		if !ok {
			t.Fatal("reader wait stopped while the transcode epoch was unavailable")
		}
	case <-time.After(time.Second):
		t.Fatal("source video did not wake reader while the transcode epoch was unavailable")
	}
	read, ok = readers.tryReadSource()
	if got := read.result.Value; !ok || got != afterUnavailableEpoch {
		t.Fatalf("source reader during unavailable transcode epoch = (%v, %v), want uninterrupted video", got, ok)
	}
}

func TestWHEPFeedReadersWakeIndependently(t *testing.T) {
	stream := core.NewStream("live/whep-independent-wake", config.StreamConfig{
		RingBufferSize: 16,
	}, config.LimitsConfig{}, core.NewEventBus())
	snapshot := stream.StartupSnapshot()
	r1, err := newWHEPFeedReaders(stream, snapshot, false, 0)
	if err != nil {
		t.Fatalf("newWHEPFeedReaders: %v", err)
	}
	r2, err := newWHEPFeedReaders(stream, snapshot, false, 0)
	if err != nil {
		t.Fatalf("newWHEPFeedReaders: %v", err)
	}
	defer r1.Close()
	defer r2.Close()

	done := make(chan struct{})
	woke1 := make(chan bool, 1)
	woke2 := make(chan bool, 1)
	r1.startWaiters(done, snapshot.GenerationDone)
	r2.startWaiters(done, snapshot.GenerationDone)
	go func() { woke1 <- r1.wait(done, snapshot.GenerationDone) }()
	go func() { woke2 <- r2.wait(done, snapshot.GenerationDone) }()
	stream.WriteFrame(avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 1, 1, []byte{1}))

	for i, woke := range []<-chan bool{woke1, woke2} {
		select {
		case ok := <-woke:
			if !ok {
				t.Fatalf("reader %d wait returned false after write", i+1)
			}
		case <-time.After(time.Second):
			t.Fatalf("reader %d did not wake after one source write", i+1)
		}
	}
	close(done)
}

func TestWHEPFeedReadersStopOnGenerationEnd(t *testing.T) {
	stream := core.NewStream("live/whep-generation", config.StreamConfig{RingBufferSize: 16}, config.LimitsConfig{}, core.NewEventBus())
	if err := stream.SetPublisher(&authorizationTestPublisher{
		id: "source", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264},
	}); err != nil {
		t.Fatal(err)
	}
	stream.WriteFrame(avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader, 0, 0, []byte{1}))
	snapshot := stream.StartupSnapshot()
	readers, err := newWHEPFeedReaders(stream, snapshot, false, 0)
	if err != nil {
		t.Fatalf("newWHEPFeedReaders: %v", err)
	}
	defer readers.Close()
	done := make(chan struct{})
	stream.RemovePublisher()
	if readers.wait(done, snapshot.GenerationDone) {
		t.Fatal("WHEP reader remained active after publisher generation ended")
	}
}

func TestWHEPTargetAudioWaiterCancellationIsNotEOF(t *testing.T) {
	sourceRing := util.NewRingBuffer[*avframe.AVFrame](2)
	targetRing := util.NewRingBuffer[*avframe.AVFrame](2)
	readers := &whepFeedReaders{
		source:      sourceRing.NewReader(),
		targetAudio: targetRing.NewReader(),
	}
	defer readers.Close()

	done := make(chan struct{})
	generationDone := make(chan struct{})
	readers.startWaiters(done, generationDone)
	readers.lifecycleMu.Lock()
	cancel := readers.waitCancel
	readers.lifecycleMu.Unlock()
	if cancel == nil {
		t.Fatal("WHEP reader waiter context was not initialized")
	}

	cancel()
	readers.waitGroup.Wait()
	if readers.activeTargetAudioEOF(done, generationDone) {
		t.Fatal("local waiter cancellation was misclassified as target-audio EOF")
	}
}

func TestWHEPInitialKeyframeGateRequiresSentCachedKeyframe(t *testing.T) {
	if whepInitialKeyframeReady("live", false) {
		t.Fatal("live mode bypassed keyframe gate without sending a cached keyframe")
	}
	if !whepInitialKeyframeReady("live", true) {
		t.Fatal("live mode did not accept a successfully sent cached keyframe")
	}
	if whepInitialKeyframeReady("realtime", true) {
		t.Fatal("realtime mode bypassed keyframe gate")
	}
}

func TestWHEPInitialMediaGateAllowsAudioOnlyStreams(t *testing.T) {
	if !whepInitialMediaReady("realtime", false, false) {
		t.Fatal("audio-only realtime playback waited for a video keyframe")
	}
	if !whepInitialMediaReady("live", false, false) {
		t.Fatal("audio-only live playback waited for a video keyframe")
	}
	if whepInitialMediaReady("realtime", false, true) {
		t.Fatal("video realtime playback bypassed the keyframe gate")
	}
}

func TestWHEPModeDefaultsToLiveSnapshot(t *testing.T) {
	if got := normalizeWHEPMode(""); got != "live" {
		t.Fatalf("empty WHEP mode = %q, want live", got)
	}
	if got := normalizeWHEPMode("invalid"); got != "live" {
		t.Fatalf("invalid WHEP mode = %q, want live", got)
	}
	if got := normalizeWHEPMode("realtime"); got != "realtime" {
		t.Fatalf("explicit realtime mode = %q, want realtime", got)
	}
}

func TestWHEPFeedStatusDistinguishesWaitingAndTerminalFailure(t *testing.T) {
	status := newWHEPFeedStatus(7, 42, "realtime")
	if got := status.Snapshot().State; got != WHEPFeedWaitingKeyframe {
		t.Fatalf("initial feed state = %q, want %q", got, WHEPFeedWaitingKeyframe)
	}

	status.SetError(WHEPFeedSampleWriteFailed, errors.New("track closed"))
	snapshot := status.Snapshot()
	if snapshot.State != WHEPFeedSampleWriteFailed {
		t.Fatalf("terminal feed state = %q, want %q", snapshot.State, WHEPFeedSampleWriteFailed)
	}
	if snapshot.LastError != "track closed" {
		t.Fatalf("feed error = %q, want track closed", snapshot.LastError)
	}
	if snapshot.Generation != 7 || snapshot.Cursor != 42 {
		t.Fatalf("feed identity = generation %d cursor %d, want generation 7 cursor 42", snapshot.Generation, snapshot.Cursor)
	}
}

func TestWHEPFeedStatusSeparatesSourceOverwriteFromTrackDrops(t *testing.T) {
	status := newWHEPFeedStatus(8, 11, "live")
	status.setExpectedMedia(true, true)
	status.recordSourceOverwrite(3)

	snapshot := status.Snapshot()
	if snapshot.SourceOverwrites != 3 {
		t.Fatalf("source overwrite count = %d, want 3", snapshot.SourceOverwrites)
	}
	if snapshot.DroppedVideo != 0 || snapshot.DroppedAudio != 0 {
		t.Fatalf("source overwrite was misclassified as track drops: %+v", snapshot)
	}
}

func TestWHEPFeedStatusReportsStableFirstMediaWait(t *testing.T) {
	status := newWHEPFeedStatus(17, 25, "live")
	createdAt := time.Date(2026, time.August, 29, 12, 0, 0, 0, time.UTC)
	status.createdAt.Store(createdAt.UnixNano())

	if got := status.Snapshot().FirstMediaWaitMS; got != 0 {
		t.Fatalf("first-media wait before media = %dms, want 0", got)
	}

	status.recordVideoAt(true, createdAt.Add(1250*time.Millisecond))
	watchdogStop := make(chan struct{})
	close(watchdogStop)
	status.watchInactivity(watchdogStop, make(chan struct{}), time.Second)
	if got := status.Snapshot().FirstMediaWaitMS; got != 1250 {
		t.Fatalf("first-media wait after watchdog start = %dms, want 1250", got)
	}

	status.recordAudioAt(true, createdAt.Add(3*time.Second))
	if got := status.Snapshot().FirstMediaWaitMS; got != 1250 {
		t.Fatalf("first-media wait after later audio = %dms, want stable 1250", got)
	}
}

func TestWHEPFeedStatusMarksNoInputAndRecoversOnMedia(t *testing.T) {
	status := newWHEPFeedStatus(8, 11, "live")
	status.MarkNoMediaInput()
	if got := status.Snapshot().State; got != WHEPFeedNoMediaInput {
		t.Fatalf("idle feed state = %q, want %q", got, WHEPFeedNoMediaInput)
	}

	status.RecordAudio(true)
	if got := status.Snapshot().State; got != WHEPFeedPlaying {
		t.Fatalf("recovered feed state = %q, want %q", got, WHEPFeedPlaying)
	}

	waiting := newWHEPFeedStatus(9, 12, "realtime")
	waiting.RecordVideo(false)
	waiting.MarkNoMediaInput()
	if got := waiting.Snapshot().State; got != WHEPFeedWaitingKeyframe {
		t.Fatalf("feed with dropped interframe state = %q, want %q", got, WHEPFeedWaitingKeyframe)
	}
}

func TestWHEPFeedStatusLogsStateTransitionsWithContext(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	status := newWHEPFeedStatus(16, 24, "live")
	status.setExpectedMedia(true, false)
	status.RecordVideo(true)
	status.RecordVideo(true)
	status.SetError(WHEPFeedSampleWriteFailed, errors.New("closed track"))

	output := logs.String()
	for _, expected := range []string{
		"msg=\"WHEP feed state changed\"",
		"previous=waiting_keyframe",
		"state=playing",
		"previous=playing",
		"state=sample_write_failed",
		"generation=16",
		"cursor=24",
		"mode=live",
	} {
		if !strings.Contains(output, expected) {
			t.Fatalf("state transition log missing %q:\n%s", expected, output)
		}
	}
	if got := strings.Count(output, "state=playing"); got != 1 {
		t.Fatalf("playing transition log count = %d, want 1:\n%s", got, output)
	}
}

func TestWHEPFeedStatusMarksPostMediaInactivityStalledAndRecovers(t *testing.T) {
	type statusTimeline interface {
		setExpectedMedia(video, audio bool)
		recordVideoAt(sent bool, now time.Time)
		checkInactivityAt(now time.Time)
	}

	status := newWHEPFeedStatus(10, 13, "live")
	timeline, ok := any(status).(statusTimeline)
	if !ok {
		t.Fatal("WHEP feed status does not support video inactivity tracking")
	}
	timeline.setExpectedMedia(true, false)
	t0 := time.Now().UTC()
	timeline.recordVideoAt(true, t0)
	timeline.checkInactivityAt(t0.Add(whepNoMediaInputTimeout))
	if got := status.Snapshot().State; got != WHEPFeedState("media_stalled") {
		t.Fatalf("inactive feed state = %q, want media_stalled", got)
	}

	timeline.recordVideoAt(true, t0.Add(whepNoMediaInputTimeout+time.Second))
	if got := status.Snapshot().State; got != WHEPFeedPlaying {
		t.Fatalf("recovered feed state = %q, want %q", got, WHEPFeedPlaying)
	}
}

func TestWHEPFeedStatusRequiresEveryExpectedMediaKindForPlayAndRecovery(t *testing.T) {
	type statusTimeline interface {
		setExpectedMedia(video, audio bool)
		recordVideoAt(sent bool, now time.Time)
		recordAudioAt(sent bool, now time.Time)
		checkInactivityAt(now time.Time)
	}

	status := newWHEPFeedStatus(12, 15, "live")
	timeline, ok := any(status).(statusTimeline)
	if !ok {
		t.Fatal("WHEP feed status does not support expected-media timeline tracking")
	}
	timeline.setExpectedMedia(true, true)
	t0 := time.Now().UTC()
	timeline.recordVideoAt(true, t0.Add(time.Second))
	if got := status.Snapshot().State; got == WHEPFeedPlaying {
		t.Fatal("video-only progress marked a mixed expected feed playing")
	}
	timeline.recordAudioAt(true, t0.Add(2*time.Second))
	if got := status.Snapshot().State; got != WHEPFeedPlaying {
		t.Fatalf("both expected media kinds state = %q, want %q", got, WHEPFeedPlaying)
	}

	timeline.checkInactivityAt(t0.Add(whepNoMediaInputTimeout + 3*time.Second))
	if got := status.Snapshot().State; got != WHEPFeedState("media_stalled") {
		t.Fatalf("stalled mixed feed state = %q, want media_stalled", got)
	}
	timeline.recordVideoAt(true, t0.Add(whepNoMediaInputTimeout+4*time.Second))
	if got := status.Snapshot().State; got != WHEPFeedState("media_stalled") {
		t.Fatalf("one-kind recovery state = %q, want media_stalled", got)
	}
	timeline.recordAudioAt(true, t0.Add(whepNoMediaInputTimeout+5*time.Second))
	if got := status.Snapshot().State; got != WHEPFeedPlaying {
		t.Fatalf("full recovery state = %q, want %q", got, WHEPFeedPlaying)
	}
}

func TestWHEPFeedStatusGivesMissingExpectedKindFullStartupGrace(t *testing.T) {
	for _, test := range []struct {
		name   string
		record func(*whepFeedStatus, time.Time)
	}{
		{
			name: "audio arrives before video",
			record: func(status *whepFeedStatus, now time.Time) {
				status.recordAudioAt(true, now)
			},
		},
		{
			name: "video arrives before audio",
			record: func(status *whepFeedStatus, now time.Time) {
				status.recordVideoAt(true, now)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			status := newWHEPFeedStatus(18, 26, "live")
			status.setExpectedMedia(true, true)
			createdAt := time.Date(2026, time.August, 29, 12, 0, 0, 0, time.UTC)
			status.createdAt.Store(createdAt.UnixNano())
			status.phase.Store(&whepFeedPhase{state: WHEPFeedWaitingKeyframe, changedAt: createdAt.UnixNano()})
			firstMediaAt := createdAt.Add(time.Second)
			test.record(status, firstMediaAt)

			status.checkInactivityAt(firstMediaAt.Add(2 * time.Second))
			if got := status.Snapshot().State; got == WHEPFeedMediaStalled {
				t.Fatalf("missing expected kind stalled after 2s, want full %s grace", whepNoMediaInputTimeout)
			}

			status.checkInactivityAt(firstMediaAt.Add(whepNoMediaInputTimeout))
			if got := status.Snapshot().State; got != WHEPFeedMediaStalled {
				t.Fatalf("missing expected kind state after grace = %q, want %q", got, WHEPFeedMediaStalled)
			}
		})
	}
}

func TestWHEPFeedStatusKeepsWaitingForFirstExpectedVideoKeyframe(t *testing.T) {
	status := newWHEPFeedStatus(15, 18, "realtime")
	status.setExpectedMedia(true, true)
	t0 := time.Now().UTC()
	status.recordAudioAt(true, t0)
	status.recordVideoAt(false, t0.Add(time.Second))
	status.checkInactivityAt(t0.Add(whepNoMediaInputTimeout + 2*time.Second))

	if got := status.Snapshot().State; got != WHEPFeedWaitingKeyframe {
		t.Fatalf("mixed feed before first video keyframe state = %q, want %q", got, WHEPFeedWaitingKeyframe)
	}
}

func TestWHEPFeedTerminalStateRejectsLateMediaAndTransportUpdates(t *testing.T) {
	status := newWHEPFeedStatus(11, 14, "live")
	status.SetError(WHEPFeedSampleWriteFailed, errors.New("track closed"))
	want := status.Snapshot()

	status.RecordVideo(true)
	status.RecordAudio(false)
	status.MarkNoMediaInput()
	status.SetTransportStats(10, 20, 30)
	if got := status.Snapshot(); got != want {
		t.Fatalf("terminal feed changed after late updates:\n got %+v\nwant %+v", got, want)
	}
}

func TestWHEPFeedTerminalStateWaitsForConcurrentUpdates(t *testing.T) {
	status := newWHEPFeedStatus(14, 17, "live")
	status.setExpectedMedia(true, true)
	start := make(chan struct{})
	var workers sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		workers.Add(1)
		go func(offset uint64) {
			defer workers.Done()
			<-start
			for index := uint64(0); index < 1000; index++ {
				status.RecordVideo(true)
				status.RecordAudio(true)
				status.SetTransportStats(index+offset, (index+offset)*100, index+offset)
				status.checkInactivityAt(time.Now().UTC())
			}
		}(uint64(worker) * 1000)
	}
	close(start)
	status.SetError(WHEPFeedSampleWriteFailed, errors.New("terminal"))
	workers.Wait()
	want := status.Snapshot()
	if want.State != WHEPFeedSampleWriteFailed || want.LastError != "terminal" {
		t.Fatalf("terminal feed = %+v", want)
	}

	status.RecordVideo(true)
	status.RecordAudio(true)
	status.SetTransportStats(^uint64(0), ^uint64(0), ^uint64(0))
	status.checkInactivityAt(time.Now().UTC().Add(whepNoMediaInputTimeout))
	if got := status.Snapshot(); got != want {
		t.Fatalf("terminal feed changed after concurrent shutdown:\n got %+v\nwant %+v", got, want)
	}
}

func TestWHEPFeedWatchdogExitsWithSessionOrGeneration(t *testing.T) {
	type statusWatchdog interface {
		watchInactivity(stop, generationDone <-chan struct{}, timeout time.Duration)
	}

	for _, terminal := range []string{"session", "generation"} {
		t.Run(terminal, func(t *testing.T) {
			status := newWHEPFeedStatus(13, 16, "live")
			watchdog, ok := any(status).(statusWatchdog)
			if !ok {
				t.Fatal("WHEP feed status does not expose a lifecycle-bound inactivity watchdog")
			}
			stop := make(chan struct{})
			generationDone := make(chan struct{})
			exited := make(chan struct{})
			go func() {
				watchdog.watchInactivity(stop, generationDone, 20*time.Millisecond)
				close(exited)
			}()
			if terminal == "session" {
				close(stop)
			} else {
				close(generationDone)
			}
			select {
			case <-exited:
			case <-time.After(time.Second):
				t.Fatal("WHEP inactivity watchdog did not exit")
			}
		})
	}
}

func TestWHEPFeedDoesNotCountUnrequestedMediaAsDropped(t *testing.T) {
	tests := []struct {
		name          string
		video         bool
		audio         bool
		wantVideoSent uint64
		wantAudioSent uint64
	}{
		{name: "audio only offer", audio: true, wantAudioSent: 1},
		{name: "video only offer", video: true, wantVideoSent: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stream := core.NewStream("live/whep-unrequested-"+test.name, config.StreamConfig{
				RingBufferSize: 16,
			}, config.LimitsConfig{}, core.NewEventBus())
			if err := stream.SetPublisher(&authorizationTestPublisher{
				id: "source",
				info: &avframe.MediaInfo{
					VideoCodec: avframe.CodecVP8,
					AudioCodec: avframe.CodecG711A,
					SampleRate: 8000,
					Channels:   1,
				},
			}); err != nil {
				t.Fatal(err)
			}
			startup := stream.StartupSnapshot()
			status := newWHEPFeedStatus(startup.Generation, startup.LiveCursor, "realtime")
			status.setExpectedMedia(test.video, test.audio)

			var videoSender, audioSender *TrackSender
			var videoCapture, audioCapture *whepRTPCapture
			if test.video {
				videoSender, videoCapture = newWHEPOverwriteSender(t, webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000}, 96)
			}
			if test.audio {
				audioSender, audioCapture = newWHEPOverwriteSender(t, webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMA, ClockRate: 8000, Channels: 1}, 8)
			}

			connected := make(chan struct{})
			close(connected)
			done := make(chan struct{})
			feedDone := make(chan struct{})
			go func() {
				defer close(feedDone)
				whepFeedLoop(stream, startup, videoSender, audioSender, done, connected, "realtime", avframe.CodecG711A, nil, status)
			}()

			stream.WriteFrame(avframe.NewAVFrame(
				avframe.MediaTypeVideo, avframe.CodecVP8, avframe.FrameTypeKeyframe,
				40, 40, []byte{0x01},
			))
			stream.WriteFrame(avframe.NewAVFrame(
				avframe.MediaTypeAudio, avframe.CodecG711A, avframe.FrameTypeInterframe,
				60, 60, []byte{0xd5},
			))

			if test.video {
				_ = videoCapture.readSample(t)
			} else {
				_ = audioCapture.readSample(t)
			}
			close(done)
			select {
			case <-feedDone:
			case <-time.After(time.Second):
				t.Fatal("WHEP feed did not stop")
			}

			snapshot := status.Snapshot()
			if snapshot.VideoFrames != test.wantVideoSent || snapshot.AudioFrames != test.wantAudioSent {
				t.Fatalf("sent media counters = %+v", snapshot)
			}
			if snapshot.DroppedVideo != 0 || snapshot.DroppedAudio != 0 {
				t.Fatalf("unrequested media counted as dropped: %+v", snapshot)
			}
		})
	}
}

func TestWHEPParameterSetsRejectInvalidH264Configuration(t *testing.T) {
	valid := buildTestAVCConfigPayload(
		[]byte{0x67, 0x42, 0x00, 0x1f, 0xe9, 0x40},
		[]byte{0x68, 0xce, 0x38, 0x80},
	)
	if !whepParameterSetsReady(avframe.CodecH264, valid) {
		t.Fatal("valid H.264 SPS/PPS configuration was rejected")
	}
	if whepParameterSetsReady(avframe.CodecH264, []byte{0x01}) {
		t.Fatal("H.264 configuration without SPS/PPS was accepted")
	}
}

func TestWHEPAudioFrameMustMatchNegotiatedCodec(t *testing.T) {
	aac := avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe, 0, 0, []byte{1})
	opus := avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecOpus, avframe.FrameTypeInterframe, 0, 0, []byte{2})

	if whepAudioFrameAllowed(aac, avframe.CodecOpus) {
		t.Fatal("AAC source frame accepted for negotiated Opus track")
	}
	if !whepAudioFrameAllowed(opus, avframe.CodecOpus) {
		t.Fatal("Opus frame rejected for negotiated Opus track")
	}
}

// TestDTSPaceDecision tests the pacing decision logic extracted from
// the feed loop. This validates the simplified behavior:
//   - sleepDur > 0 && < 1s  → should sleep
//   - sleepDur in [-1s, 0]  → should deliver immediately (no drop)
//   - |sleepDur| > 1s       → should reset pace base
func TestDTSPaceDecision(t *testing.T) {
	tests := []struct {
		name       string
		sleepDur   time.Duration
		wantAction string // "sleep", "deliver", "reset"
	}{
		{"ahead_40ms", 40 * time.Millisecond, "sleep"},
		{"ahead_500ms", 500 * time.Millisecond, "sleep"},
		{"exactly_on_time", 0, "deliver"},
		{"behind_40ms", -40 * time.Millisecond, "deliver"},
		{"behind_200ms", -200 * time.Millisecond, "deliver"},
		{"behind_500ms", -500 * time.Millisecond, "deliver"},
		{"behind_999ms", -999 * time.Millisecond, "deliver"},
		{"behind_1001ms", -1001 * time.Millisecond, "reset"},
		{"ahead_1001ms", 1001 * time.Millisecond, "reset"},
		{"behind_2s", -2 * time.Second, "reset"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dtsPaceAction(tt.sleepDur)
			if got != tt.wantAction {
				t.Errorf("dtsPaceAction(%v) = %q, want %q", tt.sleepDur, got, tt.wantAction)
			}
		})
	}
}

func TestWHEPVideoBFramePolicyIsCodecSpecific(t *testing.T) {
	bFrame := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH265, avframe.FrameTypeInterframe, 40, 33, []byte{1})

	if shouldDropWHEPVideoFrame(avframe.CodecH265, bFrame, 120) {
		t.Fatal("H.265 B-frame dropped; HEVC reference B-frames must reach the decoder")
	}
	if !shouldDropWHEPVideoFrame(avframe.CodecH264, bFrame, 120) {
		t.Fatal("H.264 B-frame retained; Chromium's H.264 WebRTC path requires it to be dropped")
	}

	keyframe := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 40, 33, []byte{2})
	if shouldDropWHEPVideoFrame(avframe.CodecH264, keyframe, 120) {
		t.Fatal("H.264 keyframe dropped because its PTS moved backward")
	}
}
