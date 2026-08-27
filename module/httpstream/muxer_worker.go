package httpstream

import (
	"bytes"
	"context"
	"encoding/binary"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/audiocodec"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/codec/aac"
	"github.com/im-pingo/liveforge/pkg/codec/opus"
	"github.com/im-pingo/liveforge/pkg/muxer/flv"
	"github.com/im-pingo/liveforge/pkg/muxer/fmp4"
	"github.com/im-pingo/liveforge/pkg/muxer/ts"
	"github.com/im-pingo/liveforge/pkg/util"
)

// copyBytes returns a newly allocated copy of the given slice.
func copyBytes(b []byte) []byte {
	c := make([]byte, len(b))
	copy(c, b)
	return c
}

// bufCopyAndReset returns a copy of buf's content and resets the buffer.
func bufCopyAndReset(buf *bytes.Buffer) []byte {
	c := copyBytes(buf.Bytes())
	buf.Reset()
	return c
}

// ensureMuxerCallbacks registers muxer start callbacks for a stream (idempotent).
func (m *Module) ensureMuxerCallbacks(stream *core.Stream) {
	m.registeredMu.Lock()
	defer m.registeredMu.Unlock()

	if m.registered[stream] {
		return
	}
	m.registered[stream] = true

	mm := stream.MuxerManager()
	mm.RegisterMuxerStart("flv", func(inst *core.MuxerInstance, s *core.Stream) {
		go m.runFLVMuxer(inst, s)
	})
	mm.RegisterMuxerStart("ts", func(inst *core.MuxerInstance, s *core.Stream) {
		go m.runTSMuxer(inst, s)
	})
	mm.RegisterMuxerStart("mp4", func(inst *core.MuxerInstance, s *core.Stream) {
		go m.runFMP4Muxer(inst, s)
	})
}

func (m *Module) runFLVMuxer(inst *core.MuxerInstance, stream *core.Stream) {
	defer inst.Buffer.Close()
	slog.Info("muxer started", "module", "httpstream", "format", "flv", "stream", stream.Key())

	snapshot, ok := waitMuxerStartup(inst, stream)
	if !ok {
		return
	}
	audioPlan := selectMuxerAudioSnapshot(stream, snapshot, isFlvCompatibleAudio)
	reader, audioPlan := muxerWorkerLiveInputSnapshot(stream, snapshot, audioPlan)
	defer reader.Close()

	muxer := flv.NewMuxer()
	var buf bytes.Buffer

	// Write FLV header as init data
	hasVideo := snapshot.VideoSequenceHeader != nil
	hasAudio := audioPlan.hasAudio()
	muxer.WriteHeader(&buf, hasVideo, hasAudio)

	// Write sequence headers into the FLV stream
	if vsh := snapshot.VideoSequenceHeader; vsh != nil {
		muxer.WriteFrame(&buf, vsh)
	}
	if ash := audioPlan.sequenceHeader; ash != nil {
		muxer.WriteFrame(&buf, ash)
	}

	inst.SetInitData(bufCopyAndReset(&buf))

	// Send the cached video GOP. Source audio is excluded when the live reader
	// transcodes it, and unsupported audio is excluded entirely.
	for _, f := range snapshot.ReplayFrames {
		if !stream.IsPublisherGeneration(snapshot.Generation) {
			return
		}
		if !audioPlan.accepts(f) {
			continue
		}
		if err := muxer.WriteFrame(&buf, f); err == nil && buf.Len() > 0 {
			inst.Buffer.Write(bufCopyAndReset(&buf))
		}
	}

	// Close reader when muxer is done so Read() unblocks promptly.
	stopReaderWatch := watchMuxerInput(reader, inst.Done, snapshot.GenerationDone)
	defer stopReaderWatch()

	for {
		frame, ok := reader.Read()
		if !ok || frame == nil {
			slog.Info("muxer stopped", "module", "httpstream", "format", "flv", "stream", stream.Key())
			return
		}
		if !stream.IsPublisherGeneration(snapshot.Generation) {
			return
		}
		if !audioPlan.accepts(frame) {
			continue
		}
		if err := muxer.WriteFrame(&buf, frame); err == nil && buf.Len() > 0 {
			inst.Buffer.Write(bufCopyAndReset(&buf))
		}
	}
}

func (m *Module) runTSMuxer(inst *core.MuxerInstance, stream *core.Stream) {
	defer inst.Buffer.Close()
	slog.Info("muxer started", "module", "httpstream", "format", "ts", "stream", stream.Key())

	snapshot, ok := waitMuxerStartup(inst, stream)
	if !ok {
		return
	}
	audioPlan := selectMuxerAudioSnapshot(stream, snapshot, isFlvCompatibleAudio)
	reader, audioPlan := muxerWorkerLiveInputSnapshot(stream, snapshot, audioPlan)
	defer reader.Close()

	// Determine codecs from sequence headers
	var videoCodec, audioCodec avframe.CodecType
	var videoSeqData, audioSeqData []byte

	if vsh := snapshot.VideoSequenceHeader; vsh != nil {
		videoCodec = vsh.Codec
		videoSeqData = vsh.Payload
	}
	if audioPlan.hasAudio() {
		audioCodec = audioPlan.codec
		if audioPlan.sequenceHeader != nil {
			audioSeqData = audioPlan.sequenceHeader.Payload
		}
	}

	muxer := ts.NewMuxer(videoCodec, audioCodec, videoSeqData, audioSeqData)

	// No init data needed for TS (PAT/PMT sent inline)

	// Send GOP cache using the same audio policy as the live reader.
	for _, f := range snapshot.ReplayFrames {
		if !stream.IsPublisherGeneration(snapshot.Generation) {
			return
		}
		if !audioPlan.accepts(f) {
			continue
		}
		if data := muxer.WriteFrame(f); len(data) > 0 {
			inst.Buffer.Write(data)
		}
	}

	// Close reader when muxer is done so Read() unblocks promptly.
	stopReaderWatch := watchMuxerInput(reader, inst.Done, snapshot.GenerationDone)
	defer stopReaderWatch()

	for {
		frame, ok := reader.Read()
		if !ok || frame == nil {
			slog.Info("muxer stopped", "module", "httpstream", "format", "ts", "stream", stream.Key())
			return
		}
		if !stream.IsPublisherGeneration(snapshot.Generation) {
			return
		}
		if frame.FrameType == avframe.FrameTypeSequenceHeader {
			if frame.MediaType.IsVideo() {
				videoCodec = frame.Codec
				videoSeqData = frame.Payload
			} else if frame.MediaType.IsAudio() {
				if audioPlan.mode == muxerAudioTranscode && frame.Codec == audioPlan.codec {
					audioPlan.sequenceHeader = frame
				} else if isFlvCompatibleAudio(frame.Codec) {
					audioPlan = muxerAudioPlan{
						mode:           muxerAudioPassthrough,
						codec:          frame.Codec,
						sequenceHeader: frame,
					}
				} else {
					continue
				}
				audioCodec = audioPlan.codec
				audioSeqData = audioPlan.sequenceHeader.Payload
			}
			muxer = ts.NewMuxer(videoCodec, audioCodec, videoSeqData, audioSeqData)
			inst.Buffer.Write(muxer.WritePATAndPMT())
			continue
		}
		if !audioPlan.accepts(frame) {
			continue
		}
		if data := muxer.WriteFrame(frame); len(data) > 0 {
			inst.Buffer.Write(data)
		}
	}
}

func (m *Module) runFMP4Muxer(inst *core.MuxerInstance, stream *core.Stream) {
	defer inst.Buffer.Close()
	slog.Info("muxer started", "module", "httpstream", "format", "fmp4", "stream", stream.Key())

	snapshot, ok := waitMuxerStartup(inst, stream)
	if !ok {
		return
	}
	audioPlan := selectFMP4AudioSnapshot(stream, snapshot)
	reader, audioPlan := muxerWorkerLiveInputSnapshot(stream, snapshot, audioPlan)
	defer reader.Close()

	var videoCodec, audioCodec avframe.CodecType
	var videoSeqHeader, audioSeqHeader *avframe.AVFrame

	if vsh := snapshot.VideoSequenceHeader; vsh != nil {
		videoCodec = vsh.Codec
		videoSeqHeader = vsh
	}
	if audioPlan.hasAudio() {
		audioCodec = audioPlan.codec
		audioSeqHeader = audioPlan.sequenceHeader
	}

	muxer := fmp4.NewMuxer(videoCodec, audioCodec)

	// Chrome requires non-zero coded_size in the fMP4 init segment.
	var videoWidth, videoHeight int
	if videoSeqHeader != nil {
		videoWidth, videoHeight = fmp4.ParseVideoDimensions(videoCodec, videoSeqHeader.Payload)
	}

	// Extract audio sample rate and channels from sequence header if available.
	audioSampleRate := 44100
	audioChannels := 2
	if audioSeqHeader != nil {
		if sr, ch := parseAudioSeqHeader(audioSeqHeader); sr > 0 {
			audioSampleRate = sr
			audioChannels = ch
		}
	}

	initSeg := muxer.Init(videoSeqHeader, audioSeqHeader, videoWidth, videoHeight, audioSampleRate, audioChannels)
	inst.SetInitData(initSeg)

	var timelineBaseDTS int64
	timelineBaseSet := false
	rebaseFrames := func(frames []*avframe.AVFrame) []*avframe.AVFrame {
		if !timelineBaseSet {
			for _, frame := range frames {
				if frame == nil || frame.FrameType == avframe.FrameTypeSequenceHeader {
					continue
				}
				if !timelineBaseSet || frame.DTS < timelineBaseDTS {
					timelineBaseDTS = frame.DTS
					timelineBaseSet = true
				}
			}
		}
		if !timelineBaseSet {
			return nil
		}

		rebased := make([]*avframe.AVFrame, 0, len(frames))
		for _, frame := range frames {
			if frame == nil {
				continue
			}
			copyFrame := *frame
			copyFrame.DTS -= timelineBaseDTS
			copyFrame.PTS -= timelineBaseDTS
			rebased = append(rebased, &copyFrame)
		}
		return rebased
	}

	// Send GOP cache as first segment using the selected audio policy.
	if len(snapshot.ReplayFrames) > 0 {
		var filtered []*avframe.AVFrame
		for _, f := range snapshot.ReplayFrames {
			if !stream.IsPublisherGeneration(snapshot.Generation) {
				return
			}
			if audioPlan.accepts(f) {
				filtered = append(filtered, f)
			}
		}
		seg := muxer.WriteSegment(rebaseFrames(filtered))
		if len(seg) > 0 {
			inst.Buffer.Write(seg)
		}
	}

	// Close reader when muxer is done so Read() unblocks promptly.
	stopReaderWatch := watchMuxerInput(reader, inst.Done, snapshot.GenerationDone)
	defer stopReaderWatch()

	const liveFragmentDurationMS = int64(200)
	var liveFrames []*avframe.AVFrame
	var liveStartDTS int64
	flushLiveFrames := func(endDTS int64) {
		if len(liveFrames) == 0 {
			return
		}
		rebasedFrames := rebaseFrames(liveFrames)
		if endDTS != 0 && timelineBaseSet {
			endDTS -= timelineBaseDTS
		}
		seg := muxer.WriteSegmentUntil(rebasedFrames, endDTS)
		if len(seg) > 0 {
			inst.Buffer.Write(seg)
		}
		liveFrames = liveFrames[:0]
	}

	for {
		frame, ok := reader.Read()
		if !ok || frame == nil {
			flushLiveFrames(0)
			slog.Info("muxer stopped", "module", "httpstream", "format", "fmp4", "stream", stream.Key())
			return
		}
		if !stream.IsPublisherGeneration(snapshot.Generation) {
			return
		}
		if frame.FrameType == avframe.FrameTypeSequenceHeader {
			continue
		}
		if !audioPlan.accepts(frame) {
			continue
		}
		if len(liveFrames) > 0 && frame.DTS-liveStartDTS >= liveFragmentDurationMS &&
			(videoCodec == 0 || frame.MediaType.IsVideo()) {
			flushLiveFrames(frame.DTS)
		}
		if len(liveFrames) == 0 {
			liveStartDTS = frame.DTS
		}
		liveFrames = append(liveFrames, frame)
	}
}

// parseAudioSeqHeader extracts sample rate and channel count from a container
// audio sequence header. Returns (0, 0) on error.
func parseAudioSeqHeader(frame *avframe.AVFrame) (sampleRate, channels int) {
	if frame == nil {
		return 0, 0
	}
	switch frame.Codec {
	case avframe.CodecAAC:
		info, err := aac.ParseAudioSpecificConfig(frame.Payload)
		if err != nil {
			return 0, 0
		}
		return info.SampleRate, info.Channels
	case avframe.CodecOpus:
		if len(frame.Payload) != 11 {
			return 0, 0
		}
		return int(binary.BigEndian.Uint32(frame.Payload[4:8])), int(frame.Payload[1])
	default:
		return 0, 0
	}
}

// isFlvCompatibleAudio returns true if the codec is FLV/TS container-compatible.
func isFlvCompatibleAudio(codec avframe.CodecType) bool {
	return codec == avframe.CodecAAC || codec == avframe.CodecMP3
}

func isFMP4CompatibleAudio(codec avframe.CodecType) bool {
	return codec == avframe.CodecAAC || codec == avframe.CodecMP3 || codec == avframe.CodecOpus
}

func selectFMP4AudioSnapshot(stream *core.Stream, snapshot core.StreamStartupSnapshot) muxerAudioPlan {
	plan := selectMuxerAudioSnapshot(stream, snapshot, isFMP4CompatibleAudio)
	if plan.mode == muxerAudioPassthrough && plan.codec == avframe.CodecOpus {
		plan.sequenceHeader = opusDOpsSequenceHeader(snapshot)
	}
	return plan
}

func opusDOpsSequenceHeader(snapshot core.StreamStartupSnapshot) *avframe.AVFrame {
	if source := snapshot.AudioSequenceHeader; source != nil && source.Codec == avframe.CodecOpus {
		if len(source.Payload) == 11 {
			return avframe.NewAVFrame(
				avframe.MediaTypeAudio, avframe.CodecOpus,
				avframe.FrameTypeSequenceHeader, source.DTS, source.PTS,
				copyBytes(source.Payload),
			)
		}
		if info, err := opus.ParseOpusHead(source.Payload); err == nil {
			return avframe.NewAVFrame(
				avframe.MediaTypeAudio, avframe.CodecOpus,
				avframe.FrameTypeSequenceHeader, source.DTS, source.PTS,
				opus.BuildDOpsBox(info),
			)
		}
	}

	sampleRate := 48000
	channels := 2
	if snapshot.MediaInfo.SampleRate > 0 {
		sampleRate = snapshot.MediaInfo.SampleRate
	}
	if snapshot.MediaInfo.Channels > 0 {
		channels = snapshot.MediaInfo.Channels
	}
	return avframe.NewAVFrame(
		avframe.MediaTypeAudio, avframe.CodecOpus,
		avframe.FrameTypeSequenceHeader, 0, 0,
		opus.BuildDOpsBox(&opus.OpusInfo{
			Version:    1,
			Channels:   channels,
			PreSkip:    312,
			SampleRate: sampleRate,
		}),
	)
}

type muxerAudioMode uint8

const (
	muxerAudioDrop muxerAudioMode = iota
	muxerAudioPassthrough
	muxerAudioTranscode
)

type muxerAudioPlan struct {
	mode           muxerAudioMode
	codec          avframe.CodecType
	sequenceHeader *avframe.AVFrame
}

func (p muxerAudioPlan) hasAudio() bool {
	return p.mode != muxerAudioDrop && p.codec != 0
}

func (p muxerAudioPlan) accepts(frame *avframe.AVFrame) bool {
	if frame == nil || !frame.MediaType.IsAudio() {
		return true
	}
	return p.hasAudio() && frame.Codec == p.codec
}

func selectMuxerAudioSnapshot(stream *core.Stream, snapshot core.StreamStartupSnapshot, compatible func(avframe.CodecType) bool) muxerAudioPlan {
	sourceCodec := snapshot.MediaInfo.AudioCodec
	if sourceCodec == 0 {
		return muxerAudioPlan{}
	}
	if compatible(sourceCodec) {
		return muxerAudioPlan{
			mode:           muxerAudioPassthrough,
			codec:          sourceCodec,
			sequenceHeader: snapshot.AudioSequenceHeader,
		}
	}
	if stream.TranscodeManager() != nil && audiocodec.Global().CanTranscode(sourceCodec, avframe.CodecAAC) {
		if sequenceHeader := aacSeqHeaderFrame(); sequenceHeader != nil {
			return muxerAudioPlan{
				mode:           muxerAudioTranscode,
				codec:          avframe.CodecAAC,
				sequenceHeader: sequenceHeader,
			}
		}
	}
	slog.Warn("muxer: unsupported audio omitted", "stream", stream.Key(), "codec", sourceCodec)
	return muxerAudioPlan{}
}

type muxerWorkerLiveInput struct {
	source    *util.RingReader[*avframe.AVFrame]
	audio     *util.RingReader[*avframe.AVFrame]
	release   func()
	frames    chan muxerWorkerFrame
	cancel    context.CancelFunc
	allDone   chan struct{}
	directAAC atomic.Bool
	audioOnce sync.Once
	closeOnce sync.Once
}

type muxerWorkerFrame struct {
	frame       *avframe.AVFrame
	transformed bool
}

func muxerWorkerLiveInputSnapshot(stream *core.Stream, snapshot core.StreamStartupSnapshot, plan muxerAudioPlan) (*muxerWorkerLiveInput, muxerAudioPlan) {
	input := &muxerWorkerLiveInput{
		source:  stream.RingBuffer().NewReaderAt(snapshot.LiveCursor),
		release: func() {},
		frames:  make(chan muxerWorkerFrame),
		allDone: make(chan struct{}),
	}
	if plan.mode == muxerAudioTranscode {
		if tm := stream.TranscodeManager(); tm != nil {
			reader, release, err := tm.GetOrCreateAudioReaderAtFromHistory(avframe.CodecAAC, snapshot.SourceCursor)
			if err == nil {
				input.audio = reader
				input.release = release
			} else {
				slog.Warn("muxer: audio transcode unavailable", "stream", stream.Key(), "error", err)
				plan = muxerAudioPlan{}
			}
		} else {
			plan = muxerAudioPlan{}
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	input.cancel = cancel
	var pumps sync.WaitGroup
	pump := func(reader *util.RingReader[*avframe.AVFrame], transformed bool, accept func(*avframe.AVFrame) bool) {
		defer pumps.Done()
		for {
			frame, ok := reader.ReadContext(ctx)
			if !ok {
				return
			}
			if !accept(frame) {
				continue
			}
			select {
			case input.frames <- muxerWorkerFrame{frame: frame, transformed: transformed}:
			case <-ctx.Done():
				return
			}
		}
	}
	pumps.Add(1)
	go pump(input.source, false, func(frame *avframe.AVFrame) bool {
		if plan.mode != muxerAudioTranscode || frame == nil || !frame.MediaType.IsAudio() {
			return true
		}
		if frame.FrameType == avframe.FrameTypeSequenceHeader && frame.Codec == plan.codec {
			input.directAAC.Store(true)
			input.closeAudio()
		}
		return input.directAAC.Load() && frame.Codec == plan.codec
	})
	if input.audio != nil {
		pumps.Add(1)
		go pump(input.audio, true, func(frame *avframe.AVFrame) bool {
			if frame == nil || !frame.MediaType.IsAudio() || frame.Codec != plan.codec {
				return false
			}
			if frame.FrameType == avframe.FrameTypeSequenceHeader {
				input.directAAC.Store(true)
				input.closeAudio()
				return false
			}
			return !input.directAAC.Load()
		})
	}
	go func() {
		pumps.Wait()
		close(input.frames)
		close(input.allDone)
	}()
	return input, plan
}

func (r *muxerWorkerLiveInput) Read() (*avframe.AVFrame, bool) {
	for {
		delivery, ok := <-r.frames
		if !ok {
			return nil, false
		}
		if delivery.transformed && r.directAAC.Load() {
			continue
		}
		return delivery.frame, true
	}
}

func (r *muxerWorkerLiveInput) closeAudio() {
	r.audioOnce.Do(func() {
		if r.audio != nil {
			r.audio.Close()
		}
		r.release()
	})
}

func (r *muxerWorkerLiveInput) Close() {
	r.closeOnce.Do(func() {
		r.cancel()
		r.source.Close()
		r.closeAudio()
		<-r.allDone
	})
}

func watchMuxerInput(input *muxerWorkerLiveInput, done, generationDone <-chan struct{}) func() {
	stop := make(chan struct{})
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-done:
			input.Close()
		case <-generationDone:
			input.Close()
		case <-stop:
		}
	}()
	var stopOnce sync.Once
	return func() {
		stopOnce.Do(func() { close(stop) })
		<-watcherDone
	}
}

// muxerLiveReader opens the live source selected by the audio plan. A failed
// transcode setup degrades to a filtered source reader so incompatible audio
// can never leak into a container whose init data declares another codec.
func muxerLiveReaderSnapshot(stream *core.Stream, snapshot core.StreamStartupSnapshot, plan muxerAudioPlan) (*util.RingReader[*avframe.AVFrame], func(), muxerAudioPlan) {
	if plan.mode == muxerAudioTranscode {
		if tm := stream.TranscodeManager(); tm != nil {
			// The HTTP muxers already replay cached video separately. The legacy
			// transcode track is intentionally started at the cached GOP source
			// cursor so it contributes both target audio history and live video;
			// callers filter the cached video portion by its video DTS watermark.
			reader, release, err := tm.GetOrCreateReaderAtFromHistory(avframe.CodecAAC, snapshot.SourceCursor)
			if err == nil {
				return reader, release, plan
			}
			slog.Warn("muxer: audio transcode unavailable", "stream", stream.Key(), "error", err)
		}
		plan = muxerAudioPlan{}
	}
	return stream.RingBuffer().NewReaderAt(snapshot.LiveCursor), func() {}, plan
}

func selectFMP4Audio(stream *core.Stream) muxerAudioPlan {
	return selectFMP4AudioSnapshot(stream, stream.StartupSnapshot())
}

func selectMuxerAudio(stream *core.Stream, compatible func(avframe.CodecType) bool) muxerAudioPlan {
	return selectMuxerAudioSnapshot(stream, stream.StartupSnapshot(), compatible)
}

func muxerLiveReader(stream *core.Stream, startPos int64, plan muxerAudioPlan) (*util.RingReader[*avframe.AVFrame], func(), muxerAudioPlan) {
	snapshot := stream.StartupSnapshot()
	snapshot.LiveCursor = startPos
	return muxerLiveReaderSnapshot(stream, snapshot, plan)
}

func waitMuxerStartup(inst *core.MuxerInstance, stream *core.Stream) (core.StreamStartupSnapshot, bool) {
	pending := stream.StartupSnapshot()
	if pending.Generation != inst.Generation || pending.GenerationDone == nil {
		return core.StreamStartupSnapshot{}, false
	}

	ctx, cancel := context.WithCancel(context.Background())
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-inst.Done:
			cancel()
		case <-pending.GenerationDone:
			cancel()
		case <-ctx.Done():
		}
	}()
	snapshot, ok := stream.WaitForStartup(ctx)
	cancel()
	<-watcherDone
	return snapshot, ok && snapshot.Generation == pending.Generation &&
		stream.IsPublisherGeneration(snapshot.Generation)
}

func cachedVideoEndDTS(frames []*avframe.AVFrame) (int64, bool) {
	var end int64
	found := false
	for _, frame := range frames {
		if frame == nil || !frame.MediaType.IsVideo() || frame.FrameType == avframe.FrameTypeSequenceHeader {
			continue
		}
		if !found || frame.DTS > end {
			end = frame.DTS
			found = true
		}
	}
	return end, found
}

func isCachedTranscodeVideo(frame *avframe.AVFrame, plan muxerAudioPlan, cachedEndDTS int64, hasCachedVideo bool) bool {
	return hasCachedVideo && plan.mode == muxerAudioTranscode && frame != nil &&
		frame.MediaType.IsVideo() && frame.FrameType != avframe.FrameTypeSequenceHeader &&
		frame.DTS <= cachedEndDTS
}

// aacSeqHeaderFrame returns a synthetic AAC sequence header AVFrame for use
// when transcoding provides AAC audio but no source sequence header exists.
func aacSeqHeaderFrame() *avframe.AVFrame {
	seqHdr := audiocodec.Global().SequenceHeader(avframe.CodecAAC)
	if seqHdr == nil {
		return nil
	}
	return avframe.NewAVFrame(
		avframe.MediaTypeAudio, avframe.CodecAAC,
		avframe.FrameTypeSequenceHeader, 0, 0, seqHdr,
	)
}
