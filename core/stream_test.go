package core

import (
	"context"
	"encoding/binary"
	"reflect"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/pkg/audiocodec"
	"github.com/im-pingo/liveforge/pkg/avframe"
)

func newTestStreamConfig() config.StreamConfig {
	return config.StreamConfig{
		GOPCache:           true,
		GOPCacheNum:        1,
		RingBufferSize:     256,
		IdleTimeout:        5 * time.Second,
		NoPublisherTimeout: 3 * time.Second,
	}
}

type testPublisher struct {
	id   string
	info *avframe.MediaInfo
}

func (p *testPublisher) ID() string                    { return p.id }
func (p *testPublisher) MediaInfo() *avframe.MediaInfo { return p.info }
func (p *testPublisher) Close() error                  { return nil }

type reentrantClosePublisher struct {
	stream *Stream
}

func (p *reentrantClosePublisher) ID() string                    { return "reentrant-close" }
func (p *reentrantClosePublisher) MediaInfo() *avframe.MediaInfo { return nil }
func (p *reentrantClosePublisher) Close() error {
	_ = p.stream.State()
	return nil
}

type typedNilTestPublisher struct{}

func (*typedNilTestPublisher) ID() string                    { return "typed-nil" }
func (*typedNilTestPublisher) MediaInfo() *avframe.MediaInfo { return nil }
func (*typedNilTestPublisher) Close() error                  { return nil }

func TestStreamStateTransitions(t *testing.T) {
	bus := NewEventBus()
	s := NewStream("live/test", newTestStreamConfig(), config.LimitsConfig{}, bus)

	if s.State() != StreamStateIdle {
		t.Fatalf("expected idle, got %v", s.State())
	}

	pub := &testPublisher{
		id:   "pub1",
		info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264, AudioCodec: avframe.CodecAAC},
	}
	err := s.SetPublisher(pub)
	if err != nil {
		t.Fatalf("SetPublisher error: %v", err)
	}
	if s.State() != StreamStatePublishing {
		t.Fatalf("expected publishing, got %v", s.State())
	}

	s.RemovePublisher()
	if s.State() != StreamStateNoPublisher {
		t.Fatalf("expected no_publisher, got %v", s.State())
	}
}

func TestStreamCloseDoesNotHoldLockWhileClosingPublisher(t *testing.T) {
	s := NewStream("live/reentrant-close", newTestStreamConfig(), config.LimitsConfig{}, NewEventBus())
	pub := &reentrantClosePublisher{stream: s}
	if err := s.SetPublisher(pub); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		s.Close()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stream close deadlocked while closing publisher")
	}
}

func TestStreamRejectsPublisherAfterClose(t *testing.T) {
	s := NewStream("live/closed", newTestStreamConfig(), config.LimitsConfig{}, NewEventBus())
	s.Close()
	if err := s.SetPublisher(&testPublisher{id: "late"}); err == nil {
		t.Fatal("SetPublisher accepted a publisher after stream close")
	}
}

func TestStreamRejectsNilPublisherWithoutMutation(t *testing.T) {
	var typedNil *typedNilTestPublisher
	tests := []struct {
		name string
		pub  Publisher
	}{
		{name: "nil"},
		{name: "typed nil", pub: typedNil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewStream("live/nil-publisher", newTestStreamConfig(), config.LimitsConfig{}, NewEventBus())
			defer s.Close()
			valid := &testPublisher{id: "valid", info: &avframe.MediaInfo{AudioCodec: avframe.CodecMP3}}
			if err := s.SetPublisher(valid); err != nil {
				t.Fatal(err)
			}
			if !s.RemovePublisherIf(valid) {
				t.Fatal("valid publisher was not removed")
			}

			before := s.StartupSnapshot()
			beforeNoPublisherTimer := s.noPublisherTimer
			beforeIdleTimer := s.idleTimer
			beforeCursor := s.RingBuffer().WriteCursor()

			if err := s.SetPublisher(tt.pub); err == nil {
				t.Error("SetPublisher accepted a nil publisher")
			}
			if s.State() != StreamStateNoPublisher {
				t.Errorf("state = %s after rejected publisher, want no_publisher", s.State())
			}
			if s.Publisher() != nil {
				t.Error("rejected publisher became active")
			}
			after := s.StartupSnapshot()
			if after.Generation != before.Generation || after.GenerationDone != before.GenerationDone {
				t.Errorf("rejected publisher changed generation from %d to %d", before.Generation, after.Generation)
			}
			if s.noPublisherTimer != beforeNoPublisherTimer || s.idleTimer != beforeIdleTimer {
				t.Error("rejected publisher changed stream timers")
			}
			frame := avframe.NewAVFrame(
				avframe.MediaTypeAudio, avframe.CodecMP3, avframe.FrameTypeInterframe,
				0, 0, []byte{0xff},
			)
			if s.WriteFrameForPublisher(tt.pub, frame) {
				t.Fatal("nil publisher frame was accepted")
			}
			if cursor := s.RingBuffer().WriteCursor(); cursor != beforeCursor {
				t.Fatalf("nil publisher advanced ring cursor from %d to %d", beforeCursor, cursor)
			}
		})
	}
}

func TestStreamRejectsNilFrameWithoutMutation(t *testing.T) {
	s := NewStream("live/nil-frame", newTestStreamConfig(), config.LimitsConfig{}, NewEventBus())
	pub := &testPublisher{id: "nil-frame-publisher"}
	if err := s.SetPublisher(pub); err != nil {
		t.Fatal(err)
	}
	beforeCursor := s.RingBuffer().WriteCursor()
	if s.WriteFrame(nil) {
		t.Fatal("WriteFrame accepted a nil frame")
	}
	if s.WriteFrameForPublisher(pub, nil) {
		t.Fatal("WriteFrameForPublisher accepted a nil frame")
	}
	if got := s.RingBuffer().WriteCursor(); got != beforeCursor {
		t.Fatalf("nil frame advanced ring cursor from %d to %d", beforeCursor, got)
	}
}

func TestRemovePublisherIfKeepsReplacement(t *testing.T) {
	s := NewStream("live/reorder", newTestStreamConfig(), config.LimitsConfig{}, NewEventBus())
	oldPublisher := &testPublisher{id: "old"}
	if err := s.SetPublisher(oldPublisher); err != nil {
		t.Fatal(err)
	}
	s.RemovePublisher()
	newPublisher := &testPublisher{id: "new"}
	if err := s.SetPublisher(newPublisher); err != nil {
		t.Fatal(err)
	}
	if s.RemovePublisherIf(oldPublisher) {
		t.Fatal("stale publisher removed the replacement")
	}
	if s.Publisher() != newPublisher {
		t.Fatal("replacement publisher was detached")
	}
}

func TestStreamRejectsPublisherIDReuseAfterInterveningGeneration(t *testing.T) {
	cfg := newTestStreamConfig()
	cfg.NoPublisherTimeout = time.Hour
	s := NewStream("live/reused-publisher-id", cfg, config.LimitsConfig{}, NewEventBus())
	tm := NewTranscodeManager(s, audiocodec.Global(), cfg.RingBufferSize)
	SetTranscodeManagerForTest(s, tm)
	firstA := &testPublisher{
		id:   "publisher-a",
		info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264},
	}
	if err := s.SetPublisher(firstA); err != nil {
		t.Fatal(err)
	}
	if !s.RemovePublisherIf(firstA) {
		t.Fatal("first publisher A was not removed")
	}

	publisherB := &testPublisher{
		id:   "publisher-b",
		info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264, AudioCodec: avframe.CodecG711U},
	}
	if err := s.SetPublisher(publisherB); err != nil {
		t.Fatal(err)
	}
	videoHeader := avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader,
		0, 0, []byte{0x67, 0x64, 0x00, 0x1f},
	)
	audioHeader := avframe.NewAVFrame(
		avframe.MediaTypeAudio, avframe.CodecG711U, avframe.FrameTypeSequenceHeader,
		0, 0, []byte{0x00, 0x01},
	)
	keyframe := avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe,
		100, 100, []byte{0x65, 0x01, 0x02},
	)
	audio := avframe.NewAVFrame(
		avframe.MediaTypeAudio, avframe.CodecG711U, avframe.FrameTypeInterframe,
		120, 120, []byte{0x11, 0x22},
	)
	interframe := avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe,
		140, 140, []byte{0x41},
	)
	for _, frame := range []*avframe.AVFrame{videoHeader, audioHeader, keyframe, audio, interframe} {
		if !s.WriteFrameForPublisher(publisherB, frame) {
			t.Fatalf("publisher B frame was rejected: %+v", frame)
		}
	}
	_, releaseTranscode, err := tm.GetOrCreateReaderAt(avframe.CodecG711A, s.GOPCacheSourceStart())
	if err != nil {
		t.Fatal(err)
	}
	defer releaseTranscode()
	if !s.RemovePublisherIf(publisherB) {
		t.Fatal("publisher B was not removed")
	}

	type stableStatsSnapshot struct {
		BytesIn     int64
		VideoFrames int64
		AudioFrames int64
		StartTime   time.Time
	}
	stableStats := func() stableStatsSnapshot {
		stats := s.Stats()
		return stableStatsSnapshot{
			BytesIn: stats.BytesIn, VideoFrames: stats.VideoFrames,
			AudioFrames: stats.AudioFrames, StartTime: stats.StartTime,
		}
	}
	type transcodeTrackSnapshot struct {
		Track              *TranscodedTrack
		TrackCount         int
		AudioTrackCount    int
		TargetCodec        avframe.CodecType
		SourceStart        int64
		SubscriberCount    int
		GenerationDone     <-chan struct{}
		GenerationBoundary *streamGenerationBoundary
	}
	transcodeState := func() transcodeTrackSnapshot {
		tm.mu.Lock()
		defer tm.mu.Unlock()
		track := tm.tracks[avframe.CodecG711A]
		snapshot := transcodeTrackSnapshot{
			Track: track, TrackCount: len(tm.tracks), AudioTrackCount: len(tm.audioTracks),
		}
		if track != nil {
			snapshot.TargetCodec = track.targetCodec
			snapshot.SourceStart = track.sourceStart
			snapshot.SubscriberCount = track.subCount
			snapshot.GenerationDone = track.generationDone
			snapshot.GenerationBoundary = track.generationBoundary
		}
		return snapshot
	}

	before := s.StartupSnapshot()
	beforeNoPublisherTimer := s.noPublisherTimer
	beforeIdleTimer := s.idleTimer
	beforeCursor := s.RingBuffer().WriteCursor()
	beforeGOP := s.GOPCache()
	beforeGOPDetail := s.GOPCacheDetail()
	beforeVideoHeader := s.VideoSeqHeader()
	beforeAudioHeader := s.AudioSeqHeader()
	beforeStats := stableStats()
	beforeTranscode := transcodeState()
	if len(beforeGOP) == 0 || beforeVideoHeader == nil || beforeAudioHeader == nil {
		t.Fatal("publisher B did not populate retained GOP and sequence-header state")
	}
	if beforeStats.BytesIn == 0 || beforeStats.VideoFrames == 0 || beforeStats.AudioFrames == 0 {
		t.Fatalf("publisher B did not populate stream stats: %+v", beforeStats)
	}
	if beforeTranscode.Track == nil || beforeTranscode.SubscriberCount != 1 {
		t.Fatalf("publisher B did not populate transcode state: %+v", beforeTranscode)
	}
	reusedA := &testPublisher{
		id:   firstA.ID(),
		info: &avframe.MediaInfo{VideoCodec: avframe.CodecH265},
	}
	if err := s.SetPublisher(reusedA); err == nil {
		t.Fatal("SetPublisher accepted A -> B -> A publisher ID reuse")
	}
	if s.Publisher() != nil || s.State() != StreamStateNoPublisher {
		t.Fatal("rejected publisher ID reuse mutated stream ownership")
	}
	after := s.StartupSnapshot()
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("rejected publisher ID reuse mutated startup state: before=%+v after=%+v", before, after)
	}
	if s.noPublisherTimer != beforeNoPublisherTimer || s.idleTimer != beforeIdleTimer {
		t.Fatal("rejected publisher ID reuse changed stream timers")
	}
	if got := s.RingBuffer().WriteCursor(); got != beforeCursor {
		t.Fatalf("rejected publisher ID reuse advanced ring cursor from %d to %d", beforeCursor, got)
	}
	if got := s.GOPCache(); !reflect.DeepEqual(got, beforeGOP) {
		t.Fatalf("rejected publisher ID reuse changed GOP cache: before=%+v after=%+v", beforeGOP, got)
	}
	if got := s.GOPCacheDetail(); got != beforeGOPDetail {
		t.Fatalf("rejected publisher ID reuse changed GOP detail: before=%+v after=%+v", beforeGOPDetail, got)
	}
	if got := s.VideoSeqHeader(); got != beforeVideoHeader || !reflect.DeepEqual(got, beforeVideoHeader) {
		t.Fatalf("rejected publisher ID reuse changed video sequence header: before=%+v after=%+v", beforeVideoHeader, got)
	}
	if got := s.AudioSeqHeader(); got != beforeAudioHeader || !reflect.DeepEqual(got, beforeAudioHeader) {
		t.Fatalf("rejected publisher ID reuse changed audio sequence header: before=%+v after=%+v", beforeAudioHeader, got)
	}
	if got := stableStats(); got != beforeStats {
		t.Fatalf("rejected publisher ID reuse changed stable stats: before=%+v after=%+v", beforeStats, got)
	}
	if got := transcodeState(); !reflect.DeepEqual(got, beforeTranscode) {
		t.Fatalf("rejected publisher ID reuse changed transcode state: before=%+v after=%+v", beforeTranscode, got)
	}

	active := &testPublisher{id: "publisher-c", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}
	if err := s.SetPublisher(active); err != nil {
		t.Fatal(err)
	}
	activeCursor := s.RingBuffer().WriteCursor()
	staleFrame := avframe.NewAVFrame(
		avframe.MediaTypeVideo,
		avframe.CodecH264,
		avframe.FrameTypeKeyframe,
		0,
		0,
		[]byte{0x65},
	)
	if s.WriteFrameForPublisher(firstA, staleFrame) {
		t.Fatal("stale publisher A frame was accepted after rejected ID reuse")
	}
	if got := s.RingBuffer().WriteCursor(); got != activeCursor {
		t.Fatalf("stale publisher A advanced ring cursor from %d to %d", activeCursor, got)
	}
	staleActivityRan := false
	if s.WithActivePublisher(firstA, func() { staleActivityRan = true }) {
		t.Fatal("stale publisher A activity was accepted after rejected ID reuse")
	}
	if staleActivityRan {
		t.Fatal("stale publisher A activity callback ran")
	}
	if s.RemovePublisherIf(firstA) {
		t.Fatal("stale publisher A removed the active publisher")
	}
	if s.Publisher() != active || s.State() != StreamStatePublishing {
		t.Fatal("stale publisher A callbacks changed active ownership")
	}
}

func TestRemovePublisherIfDoesNotRepeatCleanupForDetachedPublisher(t *testing.T) {
	cfg := newTestStreamConfig()
	cfg.NoPublisherTimeout = time.Second
	s := NewStream("live/idempotent-remove", cfg, config.LimitsConfig{}, NewEventBus())
	pub := &testPublisher{id: "publisher"}
	if err := s.SetPublisher(pub); err != nil {
		t.Fatal(err)
	}
	if !s.RemovePublisherIf(pub) {
		t.Fatal("active publisher was not removed")
	}
	timer := s.noPublisherTimer
	if timer == nil {
		t.Fatal("first removal did not start no-publisher timer")
	}
	if s.RemovePublisherIf(pub) {
		t.Fatal("detached publisher repeated stream cleanup")
	}
	if s.noPublisherTimer != timer {
		t.Fatal("detached publisher reset the no-publisher timer")
	}
}

func TestWithActivePublisherLinearizesActivityAndReplacement(t *testing.T) {
	s := NewStream("live/linearized-activity", newTestStreamConfig(), config.LimitsConfig{}, NewEventBus())
	oldPublisher := &testPublisher{id: "old"}
	if err := s.SetPublisher(oldPublisher); err != nil {
		t.Fatal(err)
	}
	activityEntered := make(chan struct{})
	releaseActivity := make(chan struct{})
	activityDone := make(chan bool, 1)
	go func() {
		activityDone <- s.WithActivePublisher(oldPublisher, func() {
			close(activityEntered)
			<-releaseActivity
		})
	}()

	select {
	case <-activityEntered:
	case <-time.After(time.Second):
		t.Fatal("active publisher activity did not start")
	}
	removeStarted := make(chan struct{})
	removeDone := make(chan bool, 1)
	go func() {
		close(removeStarted)
		removeDone <- s.RemovePublisherIf(oldPublisher)
	}()
	<-removeStarted
	select {
	case <-removeDone:
		t.Fatal("publisher replacement crossed active activity boundary")
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseActivity)
	if accepted := <-activityDone; !accepted {
		t.Fatal("active publisher activity was rejected")
	}
	if removed := <-removeDone; !removed {
		t.Fatal("old publisher was not removed after activity completed")
	}

	replacement := &testPublisher{id: "replacement"}
	if err := s.SetPublisher(replacement); err != nil {
		t.Fatal(err)
	}
	staleActivityRan := false
	if s.WithActivePublisher(oldPublisher, func() { staleActivityRan = true }) {
		t.Fatal("stale publisher activity was accepted")
	}
	if staleActivityRan {
		t.Fatal("stale publisher activity callback ran")
	}
	activeActivityRan := false
	if !s.WithActivePublisher(replacement, func() { activeActivityRan = true }) {
		t.Fatal("replacement publisher activity was rejected")
	}
	if !activeActivityRan {
		t.Fatal("replacement publisher activity callback did not run")
	}
}

func TestStreamPublisherGenerationIsolation(t *testing.T) {
	s := NewStream("live/generation", newTestStreamConfig(), config.LimitsConfig{}, NewEventBus())
	pubA := &testPublisher{
		id: "publisher-a",
		info: &avframe.MediaInfo{
			VideoCodec:          avframe.CodecH264,
			AudioCodec:          avframe.CodecAAC,
			VideoSequenceHeader: []byte{0x01, 0x64},
			AudioSequenceHeader: []byte{0x12, 0x10},
		},
	}
	if err := s.SetPublisher(pubA); err != nil {
		t.Fatal(err)
	}
	if !s.WriteFrameForPublisher(pubA, avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe,
		0, 0, []byte{0x65, 0x01},
	)) {
		t.Fatal("active publisher A frame was rejected")
	}

	snapshotA := s.StartupSnapshot()
	if snapshotA.Generation != 1 {
		t.Fatalf("publisher A generation = %d, want 1", snapshotA.Generation)
	}
	if snapshotA.GenerationDone == nil {
		t.Fatal("publisher A generation has no completion signal")
	}
	if !s.RemovePublisherIf(pubA) {
		t.Fatal("active publisher A was not removed")
	}
	select {
	case <-snapshotA.GenerationDone:
	default:
		t.Fatal("publisher A generation completion signal was not closed")
	}
	// Repeated removal must not close an already-closed generation channel again.
	s.RemovePublisher()

	pubB := &testPublisher{id: "publisher-b", info: &avframe.MediaInfo{AudioCodec: avframe.CodecMP3}}
	if err := s.SetPublisher(pubB); err != nil {
		t.Fatal(err)
	}
	staleFrame := avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe,
		40, 40, []byte{0x65, 0x02},
	)
	if s.WriteFrameForPublisher(pubA, staleFrame) {
		t.Fatal("stale publisher frame was accepted")
	}
	freshFrame := avframe.NewAVFrame(
		avframe.MediaTypeAudio, avframe.CodecMP3, avframe.FrameTypeInterframe,
		0, 0, []byte{0xff, 0xfb},
	)
	if !s.WriteFrameForPublisher(pubB, freshFrame) {
		t.Fatal("active publisher frame was rejected")
	}

	snapshotB := s.StartupSnapshot()
	if snapshotB.Generation != 2 || len(snapshotB.ReplayFrames) != 0 ||
		snapshotB.VideoSequenceHeader != nil || snapshotB.AudioSequenceHeader != nil {
		t.Fatalf("replacement snapshot leaked old generation: %+v", snapshotB)
	}
	if snapshotB.LiveCursor < snapshotB.GenerationStartCursor {
		t.Fatalf("live cursor %d precedes generation start %d", snapshotB.LiveCursor, snapshotB.GenerationStartCursor)
	}
	if snapshotB.GenerationDone == nil || snapshotB.GenerationDone == snapshotA.GenerationDone {
		t.Fatal("replacement publisher did not receive a new generation completion signal")
	}
	select {
	case <-snapshotB.GenerationDone:
		t.Fatal("replacement publisher generation completion signal is already closed")
	default:
	}
	if !s.IsPublisherGeneration(snapshotB.Generation) {
		t.Fatal("active publisher generation was not recognized")
	}
	if s.IsPublisherGeneration(snapshotA.Generation) {
		t.Fatal("retired publisher generation remained active")
	}
}

func TestStreamStartupSnapshotRetainsGenerationEndCursor(t *testing.T) {
	s := NewStream("live/generation-end-cursor", newTestStreamConfig(), config.LimitsConfig{}, NewEventBus())
	pubA := &testPublisher{id: "publisher-a", info: &avframe.MediaInfo{AudioCodec: avframe.CodecG711A}}
	if err := s.SetPublisher(pubA); err != nil {
		t.Fatal(err)
	}
	snapshotA := s.StartupSnapshot()
	if _, ended := snapshotA.GenerationEndCursor(); ended {
		t.Fatal("active generation reported an end cursor")
	}
	for timestamp := int64(0); timestamp < 60; timestamp += 20 {
		s.WriteFrameForPublisher(pubA, avframe.NewAVFrame(
			avframe.MediaTypeAudio, avframe.CodecG711A, avframe.FrameTypeInterframe,
			timestamp, timestamp, []byte{1},
		))
	}
	wantEnd := s.RingBuffer().WriteCursor()
	s.RemovePublisher()
	if got, ended := snapshotA.GenerationEndCursor(); !ended || got != wantEnd {
		t.Fatalf("old generation end cursor = (%d, %v), want (%d, true)", got, ended, wantEnd)
	}

	pubB := &testPublisher{id: "publisher-b", info: &avframe.MediaInfo{AudioCodec: avframe.CodecG711A}}
	if err := s.SetPublisher(pubB); err != nil {
		t.Fatal(err)
	}
	s.WriteFrameForPublisher(pubB, avframe.NewAVFrame(
		avframe.MediaTypeAudio, avframe.CodecG711A, avframe.FrameTypeInterframe,
		0, 0, []byte{2},
	))
	if got, _ := snapshotA.GenerationEndCursor(); got != wantEnd {
		t.Fatalf("replacement changed old generation end cursor from %d to %d", wantEnd, got)
	}
	if _, ended := s.StartupSnapshot().GenerationEndCursor(); ended {
		t.Fatal("replacement active generation inherited old end cursor")
	}
}

func TestStreamPublisherReplacementClosesGenerationBeforeReset(t *testing.T) {
	stream := NewStream("live/replacement-reset-order", newTestStreamConfig(), config.LimitsConfig{}, NewEventBus())
	defer stream.Close()
	if err := stream.SetPublisher(&testPublisher{id: "old", info: &avframe.MediaInfo{AudioCodec: avframe.CodecG711A}}); err != nil {
		t.Fatal(err)
	}
	snapshot := stream.StartupSnapshot()
	var resetCause error
	tm := NewTranscodeManager(stream, nil, 1)
	tm.tracks[avframe.CodecAAC] = &TranscodedTrack{
		generationBoundary: snapshot.generationBoundary,
		cancel:             func(err error) { resetCause = err },
	}
	SetTranscodeManagerForTest(stream, tm)
	stream.RemovePublisher()

	if err := stream.SetPublisher(&testPublisher{id: "replacement", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}); err != nil {
		t.Fatal(err)
	}
	if resetCause != nil {
		t.Fatalf("old generation transcode track reset with %v before generation close", resetCause)
	}
	if _, ended := snapshot.GenerationEndCursor(); !ended {
		t.Fatal("old publisher generation was not closed before replacement")
	}
}

func TestStreamWaitForStartupContextCancellation(t *testing.T) {
	t.Run("not ready", func(t *testing.T) {
		s := NewStream("live/startup-cancel", newTestStreamConfig(), config.LimitsConfig{}, NewEventBus())
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, ok := s.WaitForStartup(ctx); ok {
			t.Fatal("startup wait succeeded after context cancellation")
		}
	})

	t.Run("already ready", func(t *testing.T) {
		s := NewStream("live/startup-cancel-ready", newTestStreamConfig(), config.LimitsConfig{}, NewEventBus())
		if err := s.SetPublisher(&testPublisher{
			id:   "publisher",
			info: &avframe.MediaInfo{AudioCodec: avframe.CodecMP3},
		}); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, ok := s.WaitForStartup(ctx); ok {
			t.Fatal("ready startup ignored context cancellation")
		}
	})
}

func TestStreamReadinessRequiresSequenceHeadersForConfiguredCodecs(t *testing.T) {
	tests := []struct {
		name      string
		mediaInfo avframe.MediaInfo
		mediaType avframe.MediaType
		codec     avframe.CodecType
	}{
		{name: "h264", mediaInfo: avframe.MediaInfo{VideoCodec: avframe.CodecH264}, mediaType: avframe.MediaTypeVideo, codec: avframe.CodecH264},
		{name: "h265", mediaInfo: avframe.MediaInfo{VideoCodec: avframe.CodecH265}, mediaType: avframe.MediaTypeVideo, codec: avframe.CodecH265},
		{name: "aac", mediaInfo: avframe.MediaInfo{AudioCodec: avframe.CodecAAC}, mediaType: avframe.MediaTypeAudio, codec: avframe.CodecAAC},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewStream("live/requires-header/"+tt.name, newTestStreamConfig(), config.LimitsConfig{}, NewEventBus())
			pub := &testPublisher{id: tt.name, info: &tt.mediaInfo}
			if err := s.SetPublisher(pub); err != nil {
				t.Fatal(err)
			}
			if snapshot := s.StartupSnapshot(); snapshot.Ready {
				t.Fatal("generation became ready before its required sequence header")
			}
			if !s.WriteFrameForPublisher(pub, avframe.NewAVFrame(
				tt.mediaType, tt.codec, avframe.FrameTypeSequenceHeader,
				0, 0, []byte{0x01, 0x02},
			)) {
				t.Fatal("active publisher sequence header was rejected")
			}
			if snapshot := s.StartupSnapshot(); !snapshot.Ready {
				t.Fatal("generation did not become ready after its required sequence header")
			}
		})
	}
}

func TestStreamReadinessWithoutSequenceHeader(t *testing.T) {
	codecs := []avframe.CodecType{
		avframe.CodecMP3,
		avframe.CodecG711A,
		avframe.CodecG711U,
		avframe.CodecG722,
		avframe.CodecG729,
	}
	for _, codec := range codecs {
		t.Run(codec.String()+"_declared", func(t *testing.T) {
			s := NewStream("live/declared/"+codec.String(), newTestStreamConfig(), config.LimitsConfig{}, NewEventBus())
			if err := s.SetPublisher(&testPublisher{
				id:   codec.String(),
				info: &avframe.MediaInfo{AudioCodec: codec},
			}); err != nil {
				t.Fatal(err)
			}
			if snapshot := s.StartupSnapshot(); !snapshot.Ready {
				t.Fatal("declared configuration-free audio track was not ready")
			}
		})

		t.Run(codec.String()+"_observed", func(t *testing.T) {
			s := NewStream("live/observed/"+codec.String(), newTestStreamConfig(), config.LimitsConfig{}, NewEventBus())
			pub := &testPublisher{id: codec.String(), info: &avframe.MediaInfo{}}
			if err := s.SetPublisher(pub); err != nil {
				t.Fatal(err)
			}
			if snapshot := s.StartupSnapshot(); snapshot.Ready {
				t.Fatal("trackless generation became ready")
			}
			if !s.WriteFrameForPublisher(pub, avframe.NewAVFrame(
				avframe.MediaTypeAudio, codec, avframe.FrameTypeInterframe,
				0, 0, []byte{0x01},
			)) {
				t.Fatal("active publisher media frame was rejected")
			}
			if snapshot := s.StartupSnapshot(); !snapshot.Ready {
				t.Fatal("observed configuration-free audio track was not ready")
			}
		})
	}
}

func TestStreamReadinessWithoutSequenceHeaderForInBandVideo(t *testing.T) {
	for _, codec := range []avframe.CodecType{
		avframe.CodecAV1,
		avframe.CodecVP8,
		avframe.CodecVP9,
	} {
		t.Run(codec.String(), func(t *testing.T) {
			s := NewStream("live/in-band/"+codec.String(), newTestStreamConfig(), config.LimitsConfig{}, NewEventBus())
			if err := s.SetPublisher(&testPublisher{
				id:   codec.String(),
				info: &avframe.MediaInfo{VideoCodec: codec},
			}); err != nil {
				t.Fatal(err)
			}
			if snapshot := s.StartupSnapshot(); !snapshot.Ready {
				t.Fatalf("in-band codec %s was not ready without an out-of-band sequence header", codec)
			}
		})
	}
}

func TestStreamReadinessRequiresEveryKnownTrack(t *testing.T) {
	s := NewStream("live/all-tracks", newTestStreamConfig(), config.LimitsConfig{}, NewEventBus())
	pub := &testPublisher{
		id: "publisher",
		info: &avframe.MediaInfo{
			VideoCodec: avframe.CodecH264,
			AudioCodec: avframe.CodecAAC,
		},
	}
	if err := s.SetPublisher(pub); err != nil {
		t.Fatal(err)
	}
	if !s.WriteFrameForPublisher(pub, avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader,
		0, 0, []byte{0x01, 0x64},
	)) {
		t.Fatal("video sequence header was rejected")
	}
	if snapshot := s.StartupSnapshot(); snapshot.Ready {
		t.Fatal("generation became ready before every known track was configured")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	waitResult := make(chan StreamStartupSnapshot, 1)
	go func() {
		if snapshot, ok := s.WaitForStartup(ctx); ok {
			waitResult <- snapshot
		}
	}()
	if !s.WriteFrameForPublisher(pub, avframe.NewAVFrame(
		avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeSequenceHeader,
		0, 0, []byte{0x12, 0x10},
	)) {
		t.Fatal("audio sequence header was rejected")
	}
	select {
	case snapshot := <-waitResult:
		if !snapshot.Ready || snapshot.VideoSequenceHeader == nil || snapshot.AudioSequenceHeader == nil {
			t.Fatalf("startup wait returned incomplete snapshot: %+v", snapshot)
		}
	case <-ctx.Done():
		t.Fatal("startup wait did not observe readiness state change")
	}
}

func TestStreamStartupSnapshotClonesMediaInfo(t *testing.T) {
	info := &avframe.MediaInfo{
		VideoCodec:          avframe.CodecH264,
		VideoSequenceHeader: []byte{0x01, 0x64, 0x00, 0x1f},
	}
	s := NewStream("live/media-copy", newTestStreamConfig(), config.LimitsConfig{}, NewEventBus())
	if err := s.SetPublisher(&testPublisher{id: "publisher", info: info}); err != nil {
		t.Fatal(err)
	}
	info.VideoSequenceHeader[0] = 0xff

	first := s.StartupSnapshot()
	if got := first.MediaInfo.VideoSequenceHeader[0]; got != 0x01 {
		t.Fatalf("publisher mutation changed stream media info to %#x", got)
	}
	first.MediaInfo.VideoSequenceHeader[0] = 0xee
	second := s.StartupSnapshot()
	if got := second.MediaInfo.VideoSequenceHeader[0]; got != 0x01 {
		t.Fatalf("snapshot mutation changed stream media info to %#x", got)
	}
	if second.VideoSequenceHeader == nil || second.VideoSequenceHeader.Payload[0] != 0x01 {
		t.Fatal("publisher media info did not populate an isolated startup sequence header")
	}
}

func TestStreamStartupSnapshotAudioOnlyUsesLiveCursor(t *testing.T) {
	s := NewStream("live/audio-cursor", newTestStreamConfig(), config.LimitsConfig{}, NewEventBus())
	pub := &testPublisher{id: "publisher", info: &avframe.MediaInfo{AudioCodec: avframe.CodecMP3}}
	if err := s.SetPublisher(pub); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if !s.WriteFrameForPublisher(pub, avframe.NewAVFrame(
			avframe.MediaTypeAudio, avframe.CodecMP3, avframe.FrameTypeInterframe,
			int64(i*20), int64(i*20), []byte{byte(i)},
		)) {
			t.Fatal("audio frame was rejected")
		}
	}
	snapshot := s.StartupSnapshot()
	if snapshot.SourceCursor != snapshot.LiveCursor {
		t.Fatalf("audio-only source cursor = %d, want live cursor %d", snapshot.SourceCursor, snapshot.LiveCursor)
	}
	if len(snapshot.ReplayFrames) != 0 {
		t.Fatalf("audio-only startup replayed %d frames", len(snapshot.ReplayFrames))
	}
}

func TestStreamSnapshotConcurrentGOPRolloverHasNoDuplicateIdentity(t *testing.T) {
	cfg := newTestStreamConfig()
	cfg.GOPCacheNum = 3
	cfg.RingBufferSize = 4096
	s := NewStream("live/atomic-rollover", cfg, config.LimitsConfig{}, NewEventBus())
	pub := &testPublisher{id: "publisher", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}
	if err := s.SetPublisher(pub); err != nil {
		t.Fatal(err)
	}

	const frameCount = 2000
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < frameCount; i++ {
			payload := make([]byte, 8)
			binary.BigEndian.PutUint64(payload, uint64(i+1))
			frameType := avframe.FrameTypeInterframe
			if i%5 == 0 {
				frameType = avframe.FrameTypeKeyframe
			}
			if !s.WriteFrameForPublisher(pub, avframe.NewAVFrame(
				avframe.MediaTypeVideo, avframe.CodecH264, frameType,
				int64(i*40), int64(i*40), payload,
			)) {
				return
			}
			if i == 0 {
				close(started)
			}
			if i%8 == 0 {
				runtime.Gosched()
			}
		}
	}()
	<-started

	var snapshots []StreamStartupSnapshot
	for {
		snapshots = append(snapshots, s.StartupSnapshot())
		select {
		case <-done:
			goto writerDone
		default:
			runtime.Gosched()
		}
	}

writerDone:
	finalCursor := s.RingBuffer().WriteCursor()
	if len(snapshots) < 2 {
		t.Fatalf("captured %d snapshots during rollover, want at least 2", len(snapshots))
	}
	for snapshotIndex, snapshot := range snapshots {
		seen := make(map[uint64]struct{}, len(snapshot.ReplayFrames)+int(finalCursor-snapshot.LiveCursor))
		for _, frame := range snapshot.ReplayFrames {
			id := startupTestFrameIdentity(t, frame)
			if _, duplicate := seen[id]; duplicate {
				t.Fatalf("snapshot %d contains duplicate replay frame %d", snapshotIndex, id)
			}
			seen[id] = struct{}{}
		}

		reader := s.RingBuffer().NewReaderAt(snapshot.LiveCursor)
		for reader.ReadCursor() < finalCursor {
			frame, ok := reader.TryRead()
			if !ok {
				t.Fatalf("snapshot %d could not read live cursor %d before final cursor %d", snapshotIndex, reader.ReadCursor(), finalCursor)
			}
			id := startupTestFrameIdentity(t, frame)
			if _, duplicate := seen[id]; duplicate {
				t.Fatalf("snapshot %d replay/live boundary duplicated frame %d", snapshotIndex, id)
			}
			seen[id] = struct{}{}
		}
	}
}

func startupTestFrameIdentity(t *testing.T, frame *avframe.AVFrame) uint64 {
	t.Helper()
	if frame == nil || len(frame.Payload) != 8 {
		t.Fatalf("frame has invalid identity payload: %+v", frame)
	}
	return binary.BigEndian.Uint64(frame.Payload)
}

func TestStreamRejectDuplicatePublisher(t *testing.T) {
	bus := NewEventBus()
	s := NewStream("live/test", newTestStreamConfig(), config.LimitsConfig{}, bus)

	pub1 := &testPublisher{id: "pub1", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}
	pub2 := &testPublisher{id: "pub2", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}

	_ = s.SetPublisher(pub1)
	err := s.SetPublisher(pub2)
	if err == nil {
		t.Error("expected error for duplicate publisher")
	}
}

func TestStreamWriteAndReadFrames(t *testing.T) {
	bus := NewEventBus()
	s := NewStream("live/test", newTestStreamConfig(), config.LimitsConfig{}, bus)

	pub := &testPublisher{id: "pub1", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264, AudioCodec: avframe.CodecAAC}}
	_ = s.SetPublisher(pub)

	// Write frames
	keyframe := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 0, 0, []byte{0x65, 0x01})
	s.WriteFrame(keyframe)

	audio := avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe, 0, 0, []byte{0xFF, 0x01})
	s.WriteFrame(audio)

	inter := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe, 40, 40, []byte{0x41, 0x01})
	s.WriteFrame(inter)

	// Verify GOP cache
	gop := s.GOPCache()
	if len(gop) < 1 {
		t.Fatal("expected at least 1 frame in GOP cache")
	}
	if gop[0].FrameType != avframe.FrameTypeKeyframe {
		t.Error("first frame in GOP should be keyframe")
	}
}

func TestStreamNoPublisherTimeout(t *testing.T) {
	bus := NewEventBus()
	cfg := newTestStreamConfig()
	cfg.NoPublisherTimeout = 100 * time.Millisecond // short timeout for test
	s := NewStream("live/timeout", cfg, config.LimitsConfig{}, bus)

	pub := &testPublisher{id: "pub1", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}
	_ = s.SetPublisher(pub)
	s.RemovePublisher()

	if s.State() != StreamStateNoPublisher {
		t.Fatalf("expected no_publisher, got %v", s.State())
	}

	// Wait for timeout
	time.Sleep(200 * time.Millisecond)
	if s.State() != StreamStateDestroying {
		t.Errorf("expected destroying after timeout, got %v", s.State())
	}
}

func TestStreamGOPCacheDisabled(t *testing.T) {
	bus := NewEventBus()
	cfg := newTestStreamConfig()
	cfg.GOPCache = false
	s := NewStream("live/no-gop", cfg, config.LimitsConfig{}, bus)

	pub := &testPublisher{id: "pub1", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}
	_ = s.SetPublisher(pub)

	keyframe := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 0, 0, []byte{0x65})
	s.WriteFrame(keyframe)

	inter := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe, 40, 40, []byte{0x41})
	s.WriteFrame(inter)

	gop := s.GOPCache()
	if len(gop) != 0 {
		t.Errorf("expected empty GOP cache when disabled, got %d frames", len(gop))
	}
}

func TestStreamMultiGOPCache(t *testing.T) {
	bus := NewEventBus()
	cfg := newTestStreamConfig()
	cfg.GOPCache = true
	cfg.GOPCacheNum = 3
	s := NewStream("live/multi-gop", cfg, config.LimitsConfig{}, bus)

	pub := &testPublisher{id: "pub1", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}
	_ = s.SetPublisher(pub)

	// Write 4 GOPs: each has keyframe + 2 interframes
	dtsValues := [][]int64{
		{0, 40, 80},
		{120, 160, 200},
		{240, 280, 320},
		{360, 400, 440},
	}
	for _, gopDTS := range dtsValues {
		kf := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, gopDTS[0], gopDTS[0], []byte{0x65})
		s.WriteFrame(kf)
		for _, dts := range gopDTS[1:] {
			inter := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe, dts, dts, []byte{0x41})
			s.WriteFrame(inter)
		}
	}

	// With GOPCacheNum=3, should retain GOPs 2,3,4 (9 frames), first frame DTS=120
	gop := s.GOPCache()
	if len(gop) != 9 {
		t.Errorf("expected 9 cached frames, got %d", len(gop))
	}
	if len(gop) > 0 && gop[0].DTS != 120 {
		t.Errorf("expected first frame DTS=120, got %d", gop[0].DTS)
	}
	if s.GOPCacheLen() != 9 {
		t.Errorf("expected GOPCacheLen=9, got %d", s.GOPCacheLen())
	}
}

func TestStreamGOPCacheSourceStartTracksOldestCachedGOP(t *testing.T) {
	s := NewStream("live/gop-source-start", newTestStreamConfig(), config.LimitsConfig{}, NewEventBus())
	if err := s.SetPublisher(&testPublisher{id: "pub", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}); err != nil {
		t.Fatal(err)
	}
	s.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader,
		0, 0, []byte{0x01},
	))
	firstKeyframePos := s.RingBuffer().WriteCursor()
	s.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe,
		0, 0, []byte{0x65},
	))
	if got := s.GOPCacheSourceStart(); got != firstKeyframePos {
		t.Fatalf("GOP source start = %d, want %d", got, firstKeyframePos)
	}
}

func TestStreamIdleTimeout(t *testing.T) {
	bus := NewEventBus()
	cfg := newTestStreamConfig()
	cfg.IdleTimeout = 100 * time.Millisecond
	cfg.NoPublisherTimeout = 0 // disable so it doesn't interfere
	s := NewStream("live/idle", cfg, config.LimitsConfig{}, bus)

	pub := &testPublisher{id: "pub1", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}
	_ = s.SetPublisher(pub)
	_ = s.AddSubscriber("rtmp")

	// Remove both pub and sub
	s.RemovePublisher()
	s.RemoveSubscriber("rtmp")

	// Wait for idle timeout
	time.Sleep(200 * time.Millisecond)
	if s.State() != StreamStateDestroying {
		t.Errorf("expected destroying after idle timeout, got %v", s.State())
	}
}

func TestStreamIdleTimeoutCancelledBySubscriber(t *testing.T) {
	bus := NewEventBus()
	cfg := newTestStreamConfig()
	cfg.IdleTimeout = 200 * time.Millisecond
	cfg.NoPublisherTimeout = 0
	s := NewStream("live/idle-cancel", cfg, config.LimitsConfig{}, bus)

	pub := &testPublisher{id: "pub1", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}
	_ = s.SetPublisher(pub)
	s.RemovePublisher()

	// Idle timer should start (no pub, no sub)
	// Add subscriber before timeout expires
	time.Sleep(50 * time.Millisecond)
	_ = s.AddSubscriber("rtmp")

	// Wait past the original timeout
	time.Sleep(200 * time.Millisecond)
	if s.State() == StreamStateDestroying {
		t.Error("stream should not be destroying — subscriber cancelled idle timeout")
	}
}

func TestSkipTracker(t *testing.T) {
	st := NewSkipTracker(3, 10*time.Second)

	// First 3 skips should be OK (not exceed threshold)
	for i := 0; i < 3; i++ {
		if st.RecordSkip() {
			t.Errorf("skip %d should not exceed threshold", i+1)
		}
	}

	// 4th skip should exceed threshold
	if !st.RecordSkip() {
		t.Error("4th skip should exceed threshold")
	}
}

func TestSkipTrackerWindowExpiry(t *testing.T) {
	st := NewSkipTracker(3, 100*time.Millisecond)

	// Record 2 skips
	st.RecordSkip()
	st.RecordSkip()

	// Wait for window to expire
	time.Sleep(150 * time.Millisecond)

	// 3rd skip should be OK (previous ones expired)
	if st.RecordSkip() {
		t.Error("skip after window expiry should not exceed threshold")
	}
}

func TestSkipTrackerDisabled(t *testing.T) {
	st := NewSkipTracker(0, 10*time.Second)
	if st.RecordSkip() {
		t.Error("disabled tracker should never exceed threshold")
	}
}

func TestStreamMaxBitrateEnforcement(t *testing.T) {
	bus := NewEventBus()
	cfg := newTestStreamConfig()
	cfg.GOPCache = false
	limits := config.LimitsConfig{MaxBitratePerStream: 1} // 1 kbps — very low
	s := NewStream("live/bitrate-test", cfg, limits, bus)

	pub := &testPublisher{id: "pub1", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}
	_ = s.SetPublisher(pub)

	// Sequence headers must always be accepted regardless of bitrate
	seqHeader := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader, 0, 0, make([]byte, 10000))
	if ok := s.WriteFrame(seqHeader); !ok {
		t.Error("sequence header should always be accepted even when over bitrate")
	}

	// Write a large frame to push bitrate over 1 kbps
	bigFrame := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 0, 0, make([]byte, 100000))
	s.WriteFrame(bigFrame)

	// Wait so that elapsed time > 0ms for bitrate computation
	time.Sleep(2 * time.Millisecond)

	// After writing 100KB, BitrateKbps >> 1. Next frame should be rejected.
	nextFrame := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe, 40, 40, make([]byte, 1000))
	if ok := s.WriteFrame(nextFrame); ok {
		t.Error("frame should be rejected when bitrate exceeds max_bitrate_per_stream")
	}
}

func TestStreamMaxBitrateDisabled(t *testing.T) {
	bus := NewEventBus()
	cfg := newTestStreamConfig()
	cfg.GOPCache = false
	limits := config.LimitsConfig{MaxBitratePerStream: 0} // disabled
	s := NewStream("live/bitrate-disabled", cfg, limits, bus)

	pub := &testPublisher{id: "pub1", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}
	_ = s.SetPublisher(pub)

	bigFrame := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 0, 0, make([]byte, 100000))
	if ok := s.WriteFrame(bigFrame); !ok {
		t.Error("frame should be accepted when max_bitrate_per_stream is disabled")
	}
}

func BenchmarkStreamWriteFrame(b *testing.B) {
	cfg := newTestStreamConfig()
	cfg.GOPCache = false
	s := NewStream("bench/write-frame", cfg, config.LimitsConfig{}, NewEventBus())
	pub := &testPublisher{id: "bench-publisher", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}
	if err := s.SetPublisher(pub); err != nil {
		b.Fatal(err)
	}
	frame := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe, 40, 40, make([]byte, 512))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !s.WriteFrameForPublisher(pub, frame) {
			b.Fatal("benchmark frame was rejected")
		}
	}
}

func BenchmarkStreamWriteFrameWithGOPCache(b *testing.B) {
	cfg := newTestStreamConfig()
	cfg.GOPCache = true
	cfg.GOPCacheNum = 1
	s := NewStream("bench/write-frame-gop-cache", cfg, config.LimitsConfig{}, NewEventBus())
	pub := &testPublisher{id: "bench-publisher", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}
	if err := s.SetPublisher(pub); err != nil {
		b.Fatal(err)
	}
	keyframe := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 0, 0, make([]byte, 1024))
	if !s.WriteFrameForPublisher(pub, keyframe) {
		b.Fatal("benchmark keyframe was rejected")
	}
	frame := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe, 40, 40, make([]byte, 512))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		frame.DTS = int64(i + 1)
		frame.PTS = frame.DTS
		if !s.WriteFrameForPublisher(pub, frame) {
			b.Fatal("benchmark frame was rejected")
		}
	}
}

func TestStreamStateString(t *testing.T) {
	tests := []struct {
		state StreamState
		want  string
	}{
		{StreamStateIdle, "idle"},
		{StreamStateWaitingPull, "waiting_pull"},
		{StreamStatePublishing, "publishing"},
		{StreamStateNoPublisher, "no_publisher"},
		{StreamStateDestroying, "destroying"},
		{StreamState(99), "unknown"},
	}
	for _, tt := range tests {
		if got := tt.state.String(); got != tt.want {
			t.Errorf("StreamState(%d).String() = %q, want %q", tt.state, got, tt.want)
		}
	}
}

func TestStreamAccessors(t *testing.T) {
	bus := NewEventBus()
	cfg := newTestStreamConfig()
	s := NewStream("live/accessors", cfg, config.LimitsConfig{}, bus)

	if s.Key() != "live/accessors" {
		t.Errorf("expected key live/accessors, got %s", s.Key())
	}
	if s.Config().RingBufferSize != cfg.RingBufferSize {
		t.Error("Config() should return matching config")
	}
	if s.RingBuffer() == nil {
		t.Error("RingBuffer() should not be nil")
	}
	if s.MuxerManager() == nil {
		t.Error("MuxerManager() should not be nil")
	}
	if s.FeedbackRouter() == nil {
		t.Error("FeedbackRouter() should not be nil")
	}
	if s.Publisher() != nil {
		t.Error("Publisher() should be nil initially")
	}
	if s.VideoSeqHeader() != nil {
		t.Error("VideoSeqHeader() should be nil initially")
	}
	if s.AudioSeqHeader() != nil {
		t.Error("AudioSeqHeader() should be nil initially")
	}

	stats := s.Stats()
	if stats.BytesIn != 0 {
		t.Error("initial BytesIn should be 0")
	}
}

func TestStreamClose(t *testing.T) {
	bus := NewEventBus()
	cfg := newTestStreamConfig()
	s := NewStream("live/close", cfg, config.LimitsConfig{}, bus)

	pub := &testPublisher{id: "pub1", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}
	_ = s.SetPublisher(pub)

	s.Close()
	if s.State() != StreamStateDestroying {
		t.Errorf("expected destroying after Close, got %v", s.State())
	}

	// Double close should not panic
	s.Close()
}

func TestStreamDestructionIsIrreversibleAgainstLatePublisherCleanup(t *testing.T) {
	routes := []struct {
		name      string
		configure func(*config.StreamConfig)
		destroy   func(*testing.T, *Stream, Publisher)
	}{
		{
			name: "explicit close",
			configure: func(cfg *config.StreamConfig) {
				cfg.NoPublisherTimeout = 0
				cfg.IdleTimeout = 0
			},
			destroy: func(_ *testing.T, stream *Stream, _ Publisher) {
				stream.Close()
			},
		},
		{
			name: "no-publisher timeout",
			configure: func(cfg *config.StreamConfig) {
				cfg.NoPublisherTimeout = time.Millisecond
				cfg.IdleTimeout = 0
			},
			destroy: func(t *testing.T, stream *Stream, publisher Publisher) {
				t.Helper()
				if !stream.RemovePublisherIf(publisher) {
					t.Fatal("active publisher was not removed")
				}
			},
		},
		{
			name: "idle timeout",
			configure: func(cfg *config.StreamConfig) {
				cfg.NoPublisherTimeout = 0
				cfg.IdleTimeout = time.Millisecond
			},
			destroy: func(t *testing.T, stream *Stream, publisher Publisher) {
				t.Helper()
				if !stream.RemovePublisherIf(publisher) {
					t.Fatal("active publisher was not removed")
				}
			},
		},
		{
			name: "policy no-publisher timeout",
			configure: func(cfg *config.StreamConfig) {
				cfg.NoPublisherTimeout = 0
				cfg.IdleTimeout = 0
			},
			destroy: func(t *testing.T, stream *Stream, publisher Publisher) {
				t.Helper()
				if !stream.RemovePublisherIf(publisher) {
					t.Fatal("active publisher was not removed")
				}
				next := stream.Config()
				next.NoPublisherTimeout = time.Millisecond
				stream.UpdatePolicy(next, config.LimitsConfig{})
			},
		},
	}

	for _, route := range routes {
		t.Run(route.name, func(t *testing.T) {
			cfg := newTestStreamConfig()
			route.configure(&cfg)
			stream := NewStream("live/destroy-"+route.name, cfg, config.LimitsConfig{}, NewEventBus())
			publisher := &testPublisher{id: "publisher", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}
			if err := stream.SetPublisher(publisher); err != nil {
				t.Fatal(err)
			}
			if !stream.WriteFrameForPublisher(publisher, avframe.NewAVFrame(
				avframe.MediaTypeVideo,
				avframe.CodecH264,
				avframe.FrameTypeKeyframe,
				0,
				0,
				[]byte{0x65},
			)) {
				t.Fatal("active publisher frame was rejected")
			}
			cursorBeforeDestroy := stream.RingBuffer().WriteCursor()

			var destroyCalls atomic.Int32
			destroyed := make(chan struct{}, 1)
			stream.setDestroyCallback(func() {
				destroyCalls.Add(1)
				stream.RingBuffer().Close()
				select {
				case destroyed <- struct{}{}:
				default:
				}
			})
			route.destroy(t, stream, publisher)
			select {
			case <-destroyed:
			case <-time.After(time.Second):
				t.Fatal("stream destruction callback did not run")
			}

			if got := stream.State(); got != StreamStateDestroying {
				t.Fatalf("state after destruction = %s, want destroying", got)
			}
			if !stream.RingBuffer().IsClosed() {
				t.Fatal("ring remained open after destruction")
			}
			if got := destroyCalls.Load(); got != 1 {
				t.Fatalf("destroy callback calls = %d, want 1", got)
			}
			if stream.RemovePublisherIf(publisher) {
				t.Error("late conditional cleanup removed a publisher after destruction")
			}
			if got := stream.State(); got != StreamStateDestroying {
				t.Errorf("state after late conditional cleanup = %s, want destroying", got)
			}

			stream.RemovePublisher()
			if got := stream.State(); got != StreamStateDestroying {
				t.Errorf("state after late unconditional cleanup = %s, want destroying", got)
			}

			trySetPublisher := func(candidate Publisher) (err error, panicValue any) {
				defer func() { panicValue = recover() }()
				err = stream.SetPublisher(candidate)
				return err, nil
			}
			nonEmpty := &testPublisher{id: "late-non-empty", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH265}}
			if err, panicValue := trySetPublisher(nonEmpty); panicValue != nil {
				t.Errorf("non-empty publisher reattach panicked: %v", panicValue)
			} else if err == nil {
				t.Error("non-empty publisher reattached after destruction")
			}
			if got := stream.State(); got != StreamStateDestroying {
				t.Errorf("state after non-empty reattach attempt = %s, want destroying", got)
			}

			stream.RemovePublisher()
			emptyID := &testPublisher{info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}
			if err, panicValue := trySetPublisher(emptyID); panicValue != nil {
				t.Errorf("empty-ID publisher reattach panicked: %v", panicValue)
			} else if err == nil {
				t.Error("empty-ID publisher reattached after destruction")
			}
			if got := stream.State(); got != StreamStateDestroying {
				t.Errorf("state after empty-ID reattach attempt = %s, want destroying", got)
			}
			if stream.Publisher() != nil {
				t.Error("destroyed stream retained a reattached publisher")
			}
			if !stream.RingBuffer().IsClosed() {
				t.Error("late cleanup or reattach reopened the ring")
			}
			if got := stream.RingBuffer().WriteCursor(); got != cursorBeforeDestroy {
				t.Errorf("closed ring cursor = %d, want %d", got, cursorBeforeDestroy)
			}

			stream.Close()
			if got := destroyCalls.Load(); got != 1 {
				t.Errorf("destroy callback calls after late cleanup and close = %d, want 1", got)
			}
		})
	}
}

func TestStreamSubscribers(t *testing.T) {
	bus := NewEventBus()
	cfg := newTestStreamConfig()
	s := NewStream("live/subs", cfg, config.LimitsConfig{}, bus)

	_ = s.AddSubscriber("rtmp")
	_ = s.AddSubscriber("rtmp")
	_ = s.AddSubscriber("hls")

	subs := s.Subscribers()
	if subs["rtmp"] != 2 {
		t.Errorf("expected 2 rtmp subscribers, got %d", subs["rtmp"])
	}
	if subs["hls"] != 1 {
		t.Errorf("expected 1 hls subscriber, got %d", subs["hls"])
	}

	s.RemoveSubscriber("rtmp")
	subs = s.Subscribers()
	if subs["rtmp"] != 1 {
		t.Errorf("expected 1 rtmp subscriber after remove, got %d", subs["rtmp"])
	}

	s.RemoveSubscriber("rtmp")
	subs = s.Subscribers()
	if _, ok := subs["rtmp"]; ok {
		t.Error("rtmp should be removed from subscribers map when count reaches 0")
	}
}

func TestStreamMaxSubscribers(t *testing.T) {
	bus := NewEventBus()
	cfg := newTestStreamConfig()
	limits := config.LimitsConfig{MaxSubscribersPerStream: 2}
	s := NewStream("live/max-subs", cfg, limits, bus)

	if err := s.AddSubscriber("rtmp"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddSubscriber("hls"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddSubscriber("webrtc"); err == nil {
		t.Error("expected error when exceeding max_subscribers_per_stream")
	}
}

func TestStreamGenerationSubscriberReleaseDoesNotTouchReplacement(t *testing.T) {
	s := NewStream("live/generation-subs", newTestStreamConfig(), config.LimitsConfig{}, NewEventBus())
	pubA := &testPublisher{id: "publisher-a", info: &avframe.MediaInfo{AudioCodec: avframe.CodecMP3}}
	if err := s.SetPublisher(pubA); err != nil {
		t.Fatal(err)
	}
	releaseA, err := s.AddSubscriberForGeneration("sipgateway", s.StartupSnapshot().Generation)
	if err != nil {
		t.Fatal(err)
	}

	s.RemovePublisherIf(pubA)
	pubB := &testPublisher{id: "publisher-b", info: &avframe.MediaInfo{AudioCodec: avframe.CodecMP3}}
	if err := s.SetPublisher(pubB); err != nil {
		t.Fatal(err)
	}
	releaseB, err := s.AddSubscriberForGeneration("sipgateway", s.StartupSnapshot().Generation)
	if err != nil {
		t.Fatal(err)
	}

	releaseA()
	if got := s.Subscribers()["sipgateway"]; got != 1 {
		t.Fatalf("replacement subscriber count after old release = %d, want 1", got)
	}
	releaseB()
	if got := s.Subscribers()["sipgateway"]; got != 0 {
		t.Fatalf("subscriber count after replacement release = %d, want 0", got)
	}
}

func TestStreamSeqHeaderCaching(t *testing.T) {
	bus := NewEventBus()
	cfg := newTestStreamConfig()
	cfg.GOPCache = false
	s := NewStream("live/seqheader", cfg, config.LimitsConfig{}, bus)

	pub := &testPublisher{id: "pub1", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264, AudioCodec: avframe.CodecAAC}}
	_ = s.SetPublisher(pub)

	vsh := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader, 0, 0, []byte{0x67, 0x42})
	s.WriteFrame(vsh)

	ash := avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeSequenceHeader, 0, 0, []byte{0x12, 0x10})
	s.WriteFrame(ash)

	if s.VideoSeqHeader() == nil {
		t.Error("VideoSeqHeader should be cached")
	}
	if s.AudioSeqHeader() == nil {
		t.Error("AudioSeqHeader should be cached")
	}
}

func TestStreamGOPCacheDetail(t *testing.T) {
	bus := NewEventBus()
	cfg := newTestStreamConfig()
	cfg.GOPCache = true
	cfg.GOPCacheNum = 1
	s := NewStream("live/gop-detail", cfg, config.LimitsConfig{}, bus)

	pub := &testPublisher{id: "pub1", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264, AudioCodec: avframe.CodecAAC}}
	_ = s.SetPublisher(pub)

	// Empty GOP cache detail
	d := s.GOPCacheDetail()
	if d.TotalFrames != 0 || d.Generation != 0 {
		t.Errorf("empty GOP detail = %+v, want zero frames and generation", d)
	}

	// Write keyframe + interframe + late-arriving audio with an older DTS.
	kf := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 100, 100, []byte{0x65})
	s.WriteFrame(kf)
	inter := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe, 140, 140, []byte{0x41})
	s.WriteFrame(inter)
	af := avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe, 80, 80, []byte{0xFF})
	s.WriteFrame(af)

	d = s.GOPCacheDetail()
	if d.TotalFrames != 3 {
		t.Errorf("expected 3 total frames, got %d", d.TotalFrames)
	}
	if d.VideoFrames != 2 {
		t.Errorf("expected 2 video frames, got %d", d.VideoFrames)
	}
	if d.AudioFrames != 1 {
		t.Errorf("expected 1 audio frame, got %d", d.AudioFrames)
	}
	if d.DurationMs != 60 {
		t.Errorf("expected duration 60ms, got %d", d.DurationMs)
	}
	if d.Generation != 1 {
		t.Errorf("expected generation 1 after first keyframe, got %d", d.Generation)
	}

	s.WriteFrame(avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 200, 200, []byte{0x65}))
	if got := s.GOPCacheDetail().Generation; got != 2 {
		t.Errorf("expected generation 2 after second keyframe, got %d", got)
	}
}

func TestStreamGOPCacheDetailIncludesZeroDTS(t *testing.T) {
	cfg := newTestStreamConfig()
	cfg.GOPCache = true
	cfg.GOPCacheNum = 1
	stream := NewStream("live/gop-zero-dts", cfg, config.LimitsConfig{}, NewEventBus())

	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo,
		avframe.CodecH264,
		avframe.FrameTypeKeyframe,
		0,
		0,
		[]byte{0x65},
	))
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo,
		avframe.CodecH264,
		avframe.FrameTypeInterframe,
		40,
		40,
		[]byte{0x41},
	))

	if got := stream.GOPCacheDetail().DurationMs; got != 40 {
		t.Errorf("duration = %dms, want 40ms", got)
	}
}

func TestStreamGOPCacheMaxFramesKeepsPlayablePrefix(t *testing.T) {
	cfg := newTestStreamConfig()
	cfg.GOPCacheNum = 1
	cfg.GOPCacheMaxFrames = 3
	stream := NewStream("live/gop-max-frames", cfg, config.LimitsConfig{}, NewEventBus())
	if err := stream.SetPublisher(&testPublisher{id: "frames", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264, AudioCodec: avframe.CodecAAC}}); err != nil {
		t.Fatal(err)
	}

	frames := []*avframe.AVFrame{
		avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 0, 0, []byte{1}),
		avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe, 20, 20, []byte{2}),
		avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe, 40, 40, []byte{3}),
		avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe, 60, 60, []byte{4}),
	}
	for _, frame := range frames {
		stream.WriteFrame(frame)
	}

	got := stream.GOPCache()
	if len(got) != 3 || got[0] != frames[0] || got[2] != frames[2] {
		t.Fatalf("frame-bounded GOP = %v, want first three interleaved frames", got)
	}
}

func TestStreamGOPCacheMaxDurationKeepsPlayablePrefix(t *testing.T) {
	cfg := newTestStreamConfig()
	cfg.GOPCacheNum = 1
	cfg.GOPCacheMaxDuration = 40 * time.Millisecond
	stream := NewStream("live/gop-max-duration", cfg, config.LimitsConfig{}, NewEventBus())
	if err := stream.SetPublisher(&testPublisher{id: "duration", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}); err != nil {
		t.Fatal(err)
	}

	key := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 100, 100, []byte{1})
	inter := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe, 140, 140, []byte{2})
	tooLate := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe, 180, 180, []byte{3})
	stream.WriteFrame(key)
	stream.WriteFrame(inter)
	stream.WriteFrame(tooLate)

	got := stream.GOPCache()
	if len(got) != 2 || got[0] != key || got[1] != inter {
		t.Fatalf("duration-bounded GOP = %v, want frames through 140ms", got)
	}
}

func TestStreamGOPCacheMaxBytesKeepsKeyframeAndInterleavedFrames(t *testing.T) {
	cfg := newTestStreamConfig()
	cfg.GOPCacheNum = 1
	cfg.GOPCacheMaxBytes = 5
	stream := NewStream("live/gop-max-bytes", cfg, config.LimitsConfig{}, NewEventBus())
	if err := stream.SetPublisher(&testPublisher{id: "bytes", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264, AudioCodec: avframe.CodecAAC}}); err != nil {
		t.Fatal(err)
	}

	key := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 0, 0, []byte{1, 2, 3})
	audio := avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe, 20, 20, []byte{4, 5})
	video := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe, 40, 40, []byte{6})
	stream.WriteFrame(key)
	stream.WriteFrame(audio)
	stream.WriteFrame(video)

	got := stream.GOPCache()
	if len(got) != 2 || got[0] != key || got[1] != audio {
		t.Fatalf("byte-bounded GOP = %v, want keyframe plus audio", got)
	}
}

func TestStreamGOPCacheStopsAtFirstRejectedFrameUntilNextKeyframe(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*config.StreamConfig)
		rejected  *avframe.AVFrame
		later     *avframe.AVFrame
	}{
		{
			name: "bytes",
			configure: func(cfg *config.StreamConfig) {
				cfg.GOPCacheMaxBytes = 5
			},
			rejected: avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe, 40, 40, []byte{4, 5, 6}),
			later:    avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe, 60, 60, []byte{7}),
		},
		{
			name: "duration",
			configure: func(cfg *config.StreamConfig) {
				cfg.GOPCacheMaxDuration = 40 * time.Millisecond
			},
			rejected: avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe, 80, 80, []byte{4}),
			later:    avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe, 20, 20, []byte{5}),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := newTestStreamConfig()
			cfg.GOPCacheNum = 1
			test.configure(&cfg)
			stream := NewStream("live/gop-sealed-"+test.name, cfg, config.LimitsConfig{}, NewEventBus())
			if err := stream.SetPublisher(&testPublisher{id: test.name, info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264, AudioCodec: avframe.CodecAAC}}); err != nil {
				t.Fatal(err)
			}

			key := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 0, 0, []byte{1, 2, 3})
			stream.WriteFrame(key)
			stream.WriteFrame(test.rejected)
			stream.WriteFrame(test.later)
			if got := stream.GOPCache(); len(got) != 1 || got[0] != key {
				t.Fatalf("truncated GOP accepted a later frame: %v", got)
			}

			nextKey := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 100, 100, []byte{8})
			nextInter := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe, 120, 120, []byte{9})
			stream.WriteFrame(nextKey)
			stream.WriteFrame(nextInter)
			if got := stream.GOPCache(); len(got) != 2 || got[0] != nextKey || got[1] != nextInter {
				t.Fatalf("next GOP did not reopen cache: %v", got)
			}
		})
	}
}

func TestStreamGOPCacheUsesUnorderedDTSSpanForAdmission(t *testing.T) {
	cfg := newTestStreamConfig()
	cfg.GOPCacheMaxFrames = 0
	cfg.GOPCacheMaxBytes = 0
	cfg.GOPCacheMaxDuration = 100 * time.Millisecond
	stream := NewStream("live/gop-unordered-dts", cfg, config.LimitsConfig{}, NewEventBus())

	key := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 100, 100, []byte{1})
	late := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe, 140, 140, []byte{2})
	older := avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe, 0, 0, []byte{3})
	tooWide := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe, 150, 150, []byte{4})
	for _, frame := range []*avframe.AVFrame{key, late, older, tooWide} {
		stream.WriteFrame(frame)
	}

	if got := stream.GOPCache(); len(got) != 2 || got[0] != key || got[1] != late {
		t.Fatalf("unordered DTS cache = %v, want insertion-order prefix before older frame", got)
	}
	stream.WriteFrame(avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe, 50, 50, []byte{5}))
	if got := stream.GOPCache(); len(got) != 2 {
		t.Fatalf("sealed GOP accepted a frame after the exceeding span: %v", got)
	}
}

func TestStreamGOPCacheDTSExtremesDoNotOverflowDuration(t *testing.T) {
	cfg := newTestStreamConfig()
	cfg.GOPCacheMaxFrames = 0
	cfg.GOPCacheMaxBytes = 0
	cfg.GOPCacheMaxDuration = time.Millisecond
	stream := NewStream("live/gop-extreme-dts", cfg, config.LimitsConfig{}, NewEventBus())

	key := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, -1<<63, -1<<63, []byte{1})
	far := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe, 1<<63-1, 1<<63-1, []byte{2})
	stream.WriteFrame(key)
	stream.WriteFrame(far)

	if got := stream.GOPCache(); len(got) != 1 || got[0] != key {
		t.Fatalf("extreme DTS cache = %v, want only keyframe after overflow-safe rejection", got)
	}
}

func TestNewStreamAppliesHardGOPFallbackForUnvalidatedConfig(t *testing.T) {
	const wantFallbackFrames = 300
	cfg := newTestStreamConfig()
	cfg.GOPCacheMaxFrames = 0
	cfg.GOPCacheMaxBytes = 0
	cfg.GOPCacheMaxDuration = 0
	stream := NewStream("live/gop-defensive-fallback", cfg, config.LimitsConfig{}, NewEventBus())
	if got := stream.Config().GOPCacheMaxFrames; got != wantFallbackFrames {
		t.Fatalf("defensive frame bound = %d, want %d", got, wantFallbackFrames)
	}
	key := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 0, 0, []byte{1})
	stream.WriteFrame(key)
	for i := 1; i < wantFallbackFrames+20; i++ {
		stream.WriteFrame(avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe, int64(i), int64(i), []byte{2}))
	}
	if got := stream.GOPCacheLen(); got != wantFallbackFrames {
		t.Fatalf("defensive fallback cache length = %d, want %d", got, wantFallbackFrames)
	}
}

func TestStreamGOPCacheRuntimeTrimUsesUnorderedDTSSpan(t *testing.T) {
	cfg := newTestStreamConfig()
	cfg.GOPCacheMaxFrames = 10
	cfg.GOPCacheMaxBytes = 0
	cfg.GOPCacheMaxDuration = 0
	stream := NewStream("live/gop-trim-unordered-dts", cfg, config.LimitsConfig{}, NewEventBus())
	key := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 100, 100, []byte{1})
	late := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe, 140, 140, []byte{2})
	older := avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe, 0, 0, []byte{3})
	for _, frame := range []*avframe.AVFrame{key, late, older} {
		stream.WriteFrame(frame)
	}

	tightened := cfg
	tightened.GOPCacheMaxDuration = 100 * time.Millisecond
	stream.UpdatePolicy(tightened, config.LimitsConfig{})
	if got := stream.GOPCache(); len(got) != 2 || got[0] != key || got[1] != late {
		t.Fatalf("unordered DTS trim = %v, want playable prefix before older frame", got)
	}
}

func TestStreamRepublishBeforeTimeout(t *testing.T) {
	bus := NewEventBus()
	cfg := newTestStreamConfig()
	cfg.NoPublisherTimeout = 500 * time.Millisecond
	s := NewStream("live/republish", cfg, config.LimitsConfig{}, bus)

	pub1 := &testPublisher{id: "pub1", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}
	_ = s.SetPublisher(pub1)
	s.RemovePublisher()

	// Republish before timeout with same codec
	pub2 := &testPublisher{id: "pub2", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}
	err := s.SetPublisher(pub2)
	if err != nil {
		t.Fatalf("republish should succeed: %v", err)
	}
	if s.State() != StreamStatePublishing {
		t.Errorf("expected publishing after republish, got %v", s.State())
	}
}
