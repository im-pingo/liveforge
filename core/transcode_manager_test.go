package core

import (
	"bytes"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/pkg/audiocodec"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/util"
)

type partialPCMDecoder struct {
	closed bool
}

func (d *partialPCMDecoder) SetExtradata([]byte) {}
func (d *partialPCMDecoder) Decode([]byte) (*audiocodec.PCMFrame, error) {
	return &audiocodec.PCMFrame{Samples: []int16{11, 22, 33}, SampleRate: 50, Channels: 1}, nil
}
func (d *partialPCMDecoder) SampleRate() int { return 50 }
func (d *partialPCMDecoder) Channels() int   { return 1 }
func (d *partialPCMDecoder) Close()          { d.closed = true }

type observableDrainingEncoder struct {
	ring              *util.RingBuffer[transcodeOutput]
	encoded           [][]int16
	drainCalls        int
	drainedAfterClose bool
	closed            bool
}

func (e *observableDrainingEncoder) Encode(pcm *audiocodec.PCMFrame) ([]byte, error) {
	e.encoded = append(e.encoded, append([]int16(nil), pcm.Samples...))
	return []byte{0x10}, nil
}
func (e *observableDrainingEncoder) Drain() ([][]byte, error) {
	e.drainCalls++
	e.drainedAfterClose = e.drainedAfterClose || e.ring.IsClosed()
	return [][]byte{{0x20}, {0x30}}, nil
}
func (e *observableDrainingEncoder) SampleRate() int { return 50 }
func (e *observableDrainingEncoder) Channels() int   { return 1 }
func (e *observableDrainingEncoder) FrameSize() int  { return 4 }
func (e *observableDrainingEncoder) Close()          { e.closed = true }

type observableDrainingResampler struct {
	drainCalls int
	drained    bool
}

func (r *observableDrainingResampler) Resample(pcm *audiocodec.PCMFrame) *audiocodec.PCMFrame {
	return &audiocodec.PCMFrame{
		Samples: append([]int16(nil), pcm.Samples...), SampleRate: 50, Channels: 1,
	}
}

func (r *observableDrainingResampler) Drain() *audiocodec.PCMFrame {
	r.drainCalls++
	if r.drained {
		return &audiocodec.PCMFrame{SampleRate: 50, Channels: 1}
	}
	r.drained = true
	return &audiocodec.PCMFrame{Samples: []int16{44, 55}, SampleRate: 50, Channels: 1}
}

func (r *observableDrainingResampler) Close() {}

// TestTranscodeManagerZeroOverhead verifies no TranscodedTrack is created
// when the subscriber requests the same codec as the publisher.
func TestTranscodeManagerZeroOverhead(t *testing.T) {
	s := newTranscodeTestStream(avframe.CodecAAC)
	tm := s.TranscodeManager()

	reader, release, err := tm.GetOrCreateReader(avframe.CodecAAC)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer release()

	if reader == nil {
		t.Fatal("expected non-nil reader")
	}
	tm.mu.Lock()
	trackCount := len(tm.tracks)
	tm.mu.Unlock()
	if trackCount != 0 {
		t.Fatalf("expected 0 tracks, got %d", trackCount)
	}
}

// TestTranscodeManagerCreateTrack verifies a TranscodedTrack is created when codecs differ.
func TestTranscodeManagerCreateTrack(t *testing.T) {
	s := newTranscodeTestStream(avframe.CodecG711U)
	tm := s.TranscodeManager()

	reader, release, err := tm.GetOrCreateReader(avframe.CodecG711A)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer release()

	if reader == nil {
		t.Fatal("expected non-nil reader")
	}
	tm.mu.Lock()
	trackCount := len(tm.tracks)
	tm.mu.Unlock()
	if trackCount != 1 {
		t.Fatalf("expected 1 track, got %d", trackCount)
	}
}

func TestTranscodeManagerReportsTaskSnapshot(t *testing.T) {
	s := newTranscodeTestStream(avframe.CodecG711U)
	tm := s.TranscodeManager()
	reader, release, err := tm.GetOrCreateReader(avframe.CodecOpus)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer release()
	if reader == nil {
		t.Fatal("expected non-nil reader")
	}
	tasks := tm.TranscodeTasks()
	if len(tasks) != 1 {
		t.Fatalf("task count = %d, want 1", len(tasks))
	}
	if task := tasks[0]; task.SourceCodec != avframe.CodecG711U || task.TargetCodec != avframe.CodecOpus || task.AudioOnly || task.State != "running" || task.Subscribers != 1 {
		t.Fatalf("task snapshot = %+v", task)
	}
}

func TestTranscodeManagerReaderAtPreservesSnapshotStart(t *testing.T) {
	s := newTranscodeTestStream(avframe.CodecG711U)
	tm := s.TranscodeManager()
	start := s.RingBuffer().WriteCursor()
	reader, release, err := tm.GetOrCreateReaderAt(avframe.CodecG711A, start)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer release()
	if reader == nil {
		t.Fatal("expected non-nil reader")
	}

	tm.mu.Lock()
	track := tm.tracks[avframe.CodecG711A]
	got := track.sourceStart
	tm.mu.Unlock()
	if got != start {
		t.Fatalf("transcode source start = %d, want %d", got, start)
	}
}

func TestTranscodeManagerAudioReaderUsesSeparateTrack(t *testing.T) {
	s := newTranscodeTestStream(avframe.CodecG711U)
	tm := s.TranscodeManager()
	reader, release, err := tm.GetOrCreateAudioReaderAt(avframe.CodecG711A, s.StartupSnapshot())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer release()
	if reader == nil {
		t.Fatal("expected non-nil reader")
	}
	tm.mu.Lock()
	_, legacy := tm.tracks[avframe.CodecG711A]
	_, audioOnly := tm.audioTracks[avframe.CodecG711A]
	tm.mu.Unlock()
	if legacy {
		t.Fatal("audio-only reader unexpectedly created a legacy track")
	}
	if !audioOnly {
		t.Fatal("audio-only reader did not create an audio track")
	}
}

// TestTranscodeManagerSharing verifies two subscribers share one track.
func TestTranscodeManagerSharing(t *testing.T) {
	s := newTranscodeTestStream(avframe.CodecG711U)
	tm := s.TranscodeManager()

	_, release1, _ := tm.GetOrCreateReader(avframe.CodecG711A)
	_, release2, _ := tm.GetOrCreateReader(avframe.CodecG711A)

	tm.mu.Lock()
	trackCount := len(tm.tracks)
	subCount := 0
	if track, ok := tm.tracks[avframe.CodecG711A]; ok {
		subCount = track.subCount
	}
	tm.mu.Unlock()
	if trackCount != 1 {
		t.Fatalf("expected 1 shared track, got %d", trackCount)
	}
	if subCount != 2 {
		t.Fatalf("expected subCount=2, got %d", subCount)
	}

	release1()
	tm.mu.Lock()
	subCount = 0
	if track, ok := tm.tracks[avframe.CodecG711A]; ok {
		subCount = track.subCount
	}
	tm.mu.Unlock()
	if subCount != 1 {
		t.Fatalf("expected subCount=1 after release1")
	}

	release2()
	time.Sleep(50 * time.Millisecond)
	tm.mu.Lock()
	trackCount = len(tm.tracks)
	tm.mu.Unlock()
	if trackCount != 0 {
		t.Fatalf("expected 0 tracks after all releases, got %d", trackCount)
	}
}

// TestTranscodeManagerReset verifies all tracks are cleaned up on Reset.
func TestTranscodeManagerReset(t *testing.T) {
	s := newTranscodeTestStream(avframe.CodecG711U)
	tm := s.TranscodeManager()

	_, _, _ = tm.GetOrCreateReader(avframe.CodecG711A)
	_, _, _ = tm.GetOrCreateReader(avframe.CodecG722)

	tm.Reset()
	time.Sleep(50 * time.Millisecond)

	tm.mu.Lock()
	trackCount := len(tm.tracks)
	tm.mu.Unlock()
	if trackCount != 0 {
		t.Fatalf("expected 0 tracks after Reset, got %d", trackCount)
	}
}

func TestTranscodeManagerStaleReleaseDoesNotCancelReplacementTrack(t *testing.T) {
	s := newTranscodeTestStream(avframe.CodecG711U)
	tm := s.TranscodeManager()
	oldReader, releaseOld, err := tm.GetOrCreateAudioReaderAt(avframe.CodecG711A, s.StartupSnapshot())
	if err != nil {
		t.Fatal(err)
	}

	s.RemovePublisher()
	if err := s.SetPublisher(&testPublisher{
		id:   "replacement",
		info: &avframe.MediaInfo{AudioCodec: avframe.CodecG711U},
	}); err != nil {
		t.Fatal(err)
	}
	newReader, releaseNew, err := tm.GetOrCreateAudioReaderAt(avframe.CodecG711A, s.StartupSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		oldReader.Close()
		newReader.Close()
		releaseOld()
		releaseNew()
		s.RingBuffer().Close()
	})

	oldReader.Close()
	releaseOld()
	tm.mu.Lock()
	replacement := tm.audioTracks[avframe.CodecG711A]
	subCount := 0
	if replacement != nil {
		subCount = replacement.subCount
	}
	tm.mu.Unlock()
	if replacement == nil || subCount != 1 {
		t.Fatalf("stale release left replacement track/subscribers = %v/%d, want mapped/1", replacement != nil, subCount)
	}
}

func TestTranscodeManagerRejectsStaleSnapshotBeforeTrackLookup(t *testing.T) {
	s := newTranscodeTestStream(avframe.CodecG711U)
	tm := s.TranscodeManager()
	stale := s.StartupSnapshot()

	s.RemovePublisher()
	if err := s.SetPublisher(&testPublisher{
		id:   "replacement",
		info: &avframe.MediaInfo{AudioCodec: avframe.CodecG711U},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.RingBuffer().Close() })

	staleReader, staleRelease, err := tm.GetOrCreateAudioReaderAt(avframe.CodecG711A, stale)
	if err == nil {
		t.Fatal("stale snapshot acquisition succeeded before replacement track creation")
	}
	if staleReader != nil {
		staleReader.Close()
		t.Fatal("stale snapshot acquisition returned a usable reader")
	}
	staleRelease()
	tm.mu.Lock()
	_, created := tm.audioTracks[avframe.CodecG711A]
	tm.mu.Unlock()
	if created {
		t.Fatal("stale snapshot acquisition created a replacement-generation track")
	}

	current := s.StartupSnapshot()
	newReader, newRelease, err := tm.GetOrCreateAudioReaderAt(avframe.CodecG711A, current)
	if err != nil {
		t.Fatalf("current snapshot acquisition failed: %v", err)
	}
	if newReader == nil {
		t.Fatal("current snapshot acquisition returned no reader")
	}
	defer newReader.Close()
	defer newRelease()

	tm.mu.Lock()
	replacement := tm.audioTracks[avframe.CodecG711A]
	tm.mu.Unlock()
	if replacement == nil {
		t.Fatal("current snapshot acquisition did not create a track")
	}

	staleReader, staleRelease, err = tm.GetOrCreateAudioReaderAt(avframe.CodecG711A, stale)
	if err == nil {
		t.Fatal("stale snapshot acquisition reused the replacement track")
	}
	if staleReader != nil {
		staleReader.Close()
		t.Fatal("stale snapshot reuse returned a usable reader")
	}
	staleRelease()

	tm.mu.Lock()
	got := tm.audioTracks[avframe.CodecG711A]
	subCount := replacement.subCount
	tm.mu.Unlock()
	if got != replacement || subCount != 1 {
		t.Fatalf("stale snapshot changed replacement track/subscribers = %p/%d, want %p/1", got, subCount, replacement)
	}
}

// TestTranscodeManagerNoPublisher verifies error when no publisher.
func TestTranscodeManagerNoPublisher(t *testing.T) {
	cfg := config.StreamConfig{RingBufferSize: 64}
	limits := config.LimitsConfig{}
	bus := NewEventBus()
	s := NewStream("test", cfg, limits, bus)

	tm := NewTranscodeManager(s, audiocodec.Global(), 64)
	_, _, err := tm.GetOrCreateReader(avframe.CodecOpus)
	if err == nil {
		t.Fatal("expected error when no publisher")
	}
}

func TestAudioTranscodePipelineFinalizesPartialPCMBeforeRingClose(t *testing.T) {
	ring := util.NewRingBuffer[transcodeOutput](8)
	track := &TranscodedTrack{targetCodec: avframe.CodecAAC, ringBuffer: ring}
	decoder := &partialPCMDecoder{}
	encoder := &observableDrainingEncoder{ring: ring}
	resampler := &observableDrainingResampler{}
	pipeline := &audioTranscodePipeline{
		track:       track,
		sourceCodec: avframe.CodecG711U,
		sourceEpoch: 7,
		decoder:     decoder,
		encoder:     encoder,
		resampler:   resampler,
		resampled:   true,
	}
	pipeline.encode(avframe.NewAVFrame(
		avframe.MediaTypeAudio, avframe.CodecG711U, avframe.FrameTypeInterframe,
		500, 500, []byte{0xff},
	), audiocodec.SourceSpan{Begin: 10, End: 11})
	if got := ring.WriteCursor(); got != 0 {
		t.Fatalf("partial PCM wrote %d packets before finalization, want 0", got)
	}

	pipeline.finalize()
	if len(encoder.encoded) != 2 {
		t.Fatalf("encoder calls = %d, want 1 full resampler-tail frame plus 1 padded frame", len(encoder.encoded))
	}
	wantPCM := [][]int16{{11, 22, 33, 44}, {55, 0, 0, 0}}
	for frameIndex := range wantPCM {
		if got := encoder.encoded[frameIndex]; len(got) != len(wantPCM[frameIndex]) {
			t.Fatalf("encoded PCM frame %d length = %d, want %d", frameIndex, len(got), len(wantPCM[frameIndex]))
		} else {
			for i := range wantPCM[frameIndex] {
				if got[i] != wantPCM[frameIndex][i] {
					t.Fatalf("encoded PCM frame %d sample[%d] = %d, want %d", frameIndex, i, got[i], wantPCM[frameIndex][i])
				}
			}
		}
	}
	if resampler.drainCalls != 1 {
		t.Fatalf("resampler drain calls = %d, want exactly 1", resampler.drainCalls)
	}
	if encoder.drainCalls != 1 || encoder.drainedAfterClose {
		t.Fatalf("drain calls/after-close = %d/%v, want 1/false", encoder.drainCalls, encoder.drainedAfterClose)
	}
	if got := ring.WriteCursor(); got != 4 {
		t.Fatalf("finalized packet count = %d, want 2 encoded packets plus 2 delayed packets", got)
	}

	ring.Close()
	closedCursor := ring.WriteCursor()
	pipeline.finalize()
	if got := ring.WriteCursor(); got != closedCursor {
		t.Fatalf("ring cursor advanced from %d to %d after close", closedCursor, got)
	}
	if encoder.drainCalls != 1 {
		t.Fatalf("repeated finalization called drain %d times, want exactly once", encoder.drainCalls)
	}
	if resampler.drainCalls != 1 {
		t.Fatalf("repeated finalization called resampler drain %d times, want exactly once", resampler.drainCalls)
	}

	reader := ring.NewReader()
	wantDTS := []int64{500, 580, 660, 740}
	wantPayload := [][]byte{{0x10}, {0x10}, {0x20}, {0x30}}
	for i := range wantDTS {
		output, ok := reader.TryRead()
		if !ok {
			t.Fatalf("finalized output ended at packet %d", i)
		}
		frame := output.frame
		if frame.DTS != wantDTS[i] || !bytes.Equal(frame.Payload, wantPayload[i]) {
			t.Fatalf("packet %d = DTS %d payload %x, want DTS %d payload %x", i, frame.DTS, frame.Payload, wantDTS[i], wantPayload[i])
		}
		if frame.AudioCodecEpoch != 7 || frame.AudioProvenance != avframe.FrameProvenanceTranscoded {
			t.Fatalf("packet %d epoch/provenance = %d/%d, want 7/transcoded", i, frame.AudioCodecEpoch, frame.AudioProvenance)
		}
	}
	pipeline.close()
	if !decoder.closed || !encoder.closed {
		t.Fatal("pipeline close did not release decoder and encoder")
	}
}

// newTranscodeTestStream creates a test stream with a publisher of the given audio codec.
func newTranscodeTestStream(codec avframe.CodecType) *Stream {
	cfg := config.StreamConfig{RingBufferSize: 64}
	limits := config.LimitsConfig{}
	bus := NewEventBus()
	s := NewStream("test", cfg, limits, bus)
	s.transcodeManager = NewTranscodeManager(s, audiocodec.Global(), 64)

	pub := &testPublisher{
		id:   "test-pub",
		info: &avframe.MediaInfo{AudioCodec: codec},
	}
	_ = s.SetPublisher(pub)
	return s
}
