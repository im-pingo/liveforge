package httpstream

import (
	"bytes"
	"context"
	"encoding/binary"
	"log/slog"
	"sync"

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

	key := stream.Key()
	if m.registered[key] == stream.InstanceID() {
		return
	}
	m.registered[key] = stream.InstanceID()

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
	m.runFLVMuxerWithSnapshot(inst, stream, snapshot)
}

func (m *Module) runFLVMuxerWithSnapshot(inst *core.MuxerInstance, stream *core.Stream, snapshot core.StreamStartupSnapshot) {
	audioPlan := selectMuxerAudioSnapshot(stream, snapshot, isFlvCompatibleAudio)
	reader, audioPlan := muxerWorkerLiveInputSnapshot(stream, snapshot, audioPlan)
	m.runFLVMuxerInput(inst, stream, snapshot, audioPlan, reader)
}

func (m *Module) runFLVMuxerInput(
	inst *core.MuxerInstance,
	stream *core.Stream,
	snapshot core.StreamStartupSnapshot,
	audioPlan muxerAudioPlan,
	reader *muxerWorkerLiveInput,
) {
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
		result := reader.ReadResult()
		if result.Overwrite.Count > 0 {
			logMuxerInputOverwrite("flv", stream, result.Overwrite)
			return
		}
		frame := result.Frame
		if !result.OK || frame == nil {
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
	m.runTSMuxerWithSnapshot(inst, stream, snapshot)
}

func (m *Module) runTSMuxerWithSnapshot(inst *core.MuxerInstance, stream *core.Stream, snapshot core.StreamStartupSnapshot) {
	audioPlan := selectMuxerAudioSnapshot(stream, snapshot, isFlvCompatibleAudio)
	reader, audioPlan := muxerWorkerLiveInputSnapshot(stream, snapshot, audioPlan)
	m.runTSMuxerInput(inst, stream, snapshot, audioPlan, reader)
}

func (m *Module) runTSMuxerInput(
	inst *core.MuxerInstance,
	stream *core.Stream,
	snapshot core.StreamStartupSnapshot,
	audioPlan muxerAudioPlan,
	reader *muxerWorkerLiveInput,
) {
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
		result := reader.ReadResult()
		if result.Overwrite.Count > 0 {
			logMuxerInputOverwrite("ts", stream, result.Overwrite)
			return
		}
		frame := result.Frame
		if !result.OK || frame == nil {
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
	m.runFMP4MuxerWithSnapshot(inst, stream, snapshot)
}

func (m *Module) runFMP4MuxerWithSnapshot(inst *core.MuxerInstance, stream *core.Stream, snapshot core.StreamStartupSnapshot) {
	audioPlan := selectFMP4AudioSnapshot(stream, snapshot)
	reader, audioPlan := muxerWorkerLiveInputSnapshot(stream, snapshot, audioPlan)
	m.runFMP4MuxerInput(inst, stream, snapshot, audioPlan, reader)
}

func (m *Module) runFMP4MuxerInput(
	inst *core.MuxerInstance,
	stream *core.Stream,
	snapshot core.StreamStartupSnapshot,
	audioPlan muxerAudioPlan,
	reader *muxerWorkerLiveInput,
) {
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
		result := reader.ReadResult()
		if result.Overwrite.Count > 0 {
			liveFrames = nil
			logMuxerInputOverwrite("fmp4", stream, result.Overwrite)
			return
		}
		frame := result.Frame
		if !result.OK || frame == nil {
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

func logMuxerInputOverwrite(format string, stream *core.Stream, overwrite muxerWorkerOverwrite) {
	slog.Warn("muxer input continuity lost",
		"module", "httpstream",
		"format", format,
		"stream", stream.Key(),
		"input", overwrite.Input,
		"overwritten", overwrite.Count,
	)
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
	source               *util.RingReader[*avframe.AVFrame]
	audio                *util.RingReader[*avframe.AVFrame]
	release              func()
	frames               chan muxerWorkerFrame
	ctx                  context.Context
	cancel               context.CancelFunc
	allDone              chan struct{}
	terminalDone         chan struct{}
	plan                 muxerAudioPlan
	audioOwner           muxerWorkerAudioOwner
	audioEpoch           uint64
	directEpochSeen      bool
	startupHeaderHandled bool
	terminalMu           sync.Mutex
	terminal             muxerWorkerOverwrite
	terminalOnce         sync.Once
	audioOnce            sync.Once
	closeOnce            sync.Once
}

type muxerWorkerInputKind string

const (
	muxerWorkerInputDirectSource     muxerWorkerInputKind = "direct_source"
	muxerWorkerInputTransformedAudio muxerWorkerInputKind = "transformed_audio"
)

type muxerWorkerOverwrite struct {
	Input muxerWorkerInputKind
	Count int64
}

type muxerWorkerReadResult struct {
	Frame     *avframe.AVFrame
	OK        bool
	Overwrite muxerWorkerOverwrite
}

type muxerWorkerAudioOwner uint8

const (
	muxerWorkerAudioUnknown muxerWorkerAudioOwner = iota
	muxerWorkerAudioTransformed
	muxerWorkerAudioDirect
)

type muxerWorkerFrame struct {
	frame       *avframe.AVFrame
	transformed bool
}

type muxerWorkerInputHooks struct {
	beforeRead    func(muxerWorkerInputKind)
	beforeDeliver func(muxerWorkerInputKind)
}

func muxerWorkerLiveInputSnapshot(stream *core.Stream, snapshot core.StreamStartupSnapshot, plan muxerAudioPlan) (*muxerWorkerLiveInput, muxerAudioPlan) {
	source := stream.RingBuffer().NewReaderAt(snapshot.LiveCursor)
	var audio *util.RingReader[*avframe.AVFrame]
	release := func() {}
	followAudioEpochs := plan.mode == muxerAudioTranscode ||
		(plan.mode == muxerAudioPassthrough && plan.codec == avframe.CodecAAC)
	if followAudioEpochs {
		if tm := stream.TranscodeManager(); tm != nil {
			reader, releaseReader, err := tm.GetOrCreateAudioReaderAtFromHistory(avframe.CodecAAC, snapshot)
			if err == nil {
				audio = reader
				release = releaseReader
			} else {
				slog.Warn("muxer: audio transcode unavailable", "stream", stream.Key(), "error", err)
				if plan.mode == muxerAudioTranscode {
					plan = muxerAudioPlan{}
				}
			}
		} else if plan.mode == muxerAudioTranscode {
			plan = muxerAudioPlan{}
		}
	}
	if audio != nil {
		plan.mode = muxerAudioTranscode
	}
	return newMuxerWorkerLiveInput(source, audio, release, plan), plan
}

func newMuxerWorkerLiveInput(
	source, audio *util.RingReader[*avframe.AVFrame],
	release func(),
	plan muxerAudioPlan,
) *muxerWorkerLiveInput {
	return newMuxerWorkerLiveInputWithHooks(source, audio, release, plan, muxerWorkerInputHooks{})
}

func newMuxerWorkerLiveInputWithHooks(
	source, audio *util.RingReader[*avframe.AVFrame],
	release func(),
	plan muxerAudioPlan,
	hooks muxerWorkerInputHooks,
) *muxerWorkerLiveInput {
	ctx, cancel := context.WithCancel(context.Background())
	input := &muxerWorkerLiveInput{
		source:       source,
		audio:        audio,
		release:      release,
		frames:       make(chan muxerWorkerFrame),
		ctx:          ctx,
		cancel:       cancel,
		allDone:      make(chan struct{}),
		terminalDone: make(chan struct{}),
		plan:         plan,
	}
	var pumps sync.WaitGroup
	pump := func(reader *util.RingReader[*avframe.AVFrame], kind muxerWorkerInputKind, transformed bool, accept func(*avframe.AVFrame) bool) {
		defer pumps.Done()
		for {
			if hooks.beforeRead != nil {
				hooks.beforeRead(kind)
			}
			result := reader.ReadResultContext(ctx)
			if result.Overwritten > 0 {
				input.terminateOverwrite(kind, result.Overwritten)
				return
			}
			if !result.OK {
				return
			}
			frame := result.Value
			if !accept(frame) {
				continue
			}
			if hooks.beforeDeliver != nil {
				hooks.beforeDeliver(kind)
			}
			if !input.deliver(muxerWorkerFrame{frame: frame, transformed: transformed}) {
				return
			}
		}
	}
	pumps.Add(1)
	go pump(input.source, muxerWorkerInputDirectSource, false, func(frame *avframe.AVFrame) bool { return frame != nil })
	if input.audio != nil {
		pumps.Add(1)
		go pump(input.audio, muxerWorkerInputTransformedAudio, true, func(frame *avframe.AVFrame) bool {
			return frame != nil && frame.MediaType.IsAudio() && frame.Codec == plan.codec
		})
	}
	go func() {
		pumps.Wait()
		close(input.frames)
		close(input.allDone)
	}()
	return input
}

func (r *muxerWorkerLiveInput) Read() (*avframe.AVFrame, bool) {
	result := r.ReadResult()
	return result.Frame, result.OK
}

func (r *muxerWorkerLiveInput) ReadResult() muxerWorkerReadResult {
	for {
		select {
		case <-r.terminalDone:
			return r.overwriteResult()
		default:
		}

		select {
		case <-r.terminalDone:
			return r.overwriteResult()
		case delivery, ok := <-r.frames:
			if !ok {
				select {
				case <-r.terminalDone:
					return r.overwriteResult()
				default:
					return muxerWorkerReadResult{}
				}
			}
			frame := delivery.frame
			if r.plan.mode != muxerAudioTranscode || frame == nil || !frame.MediaType.IsAudio() {
				return muxerWorkerReadResult{Frame: frame, OK: true}
			}
			if !r.acceptAudioDelivery(delivery) {
				continue
			}
			return muxerWorkerReadResult{Frame: frame, OK: true}
		}
	}
}

func (r *muxerWorkerLiveInput) deliver(delivery muxerWorkerFrame) bool {
	r.terminalMu.Lock()
	defer r.terminalMu.Unlock()
	if r.terminal.Count > 0 {
		return false
	}
	select {
	case r.frames <- delivery:
		return true
	case <-r.ctx.Done():
		return false
	}
}

func (r *muxerWorkerLiveInput) terminateOverwrite(input muxerWorkerInputKind, count int64) {
	r.terminalOnce.Do(func() {
		r.terminalMu.Lock()
		r.terminal = muxerWorkerOverwrite{Input: input, Count: count}
		r.cancel()
		close(r.terminalDone)
		r.terminalMu.Unlock()
	})
}

func (r *muxerWorkerLiveInput) overwriteResult() muxerWorkerReadResult {
	<-r.allDone
	r.terminalMu.Lock()
	overwrite := r.terminal
	r.terminalMu.Unlock()
	return muxerWorkerReadResult{Overwrite: overwrite}
}

func (r *muxerWorkerLiveInput) acceptAudioDelivery(delivery muxerWorkerFrame) bool {
	frame := delivery.frame
	epoch := frame.AudioCodecEpoch
	if epoch == 0 {
		epoch = r.audioEpoch
		if epoch == 0 {
			epoch = 1
		}
	}
	if epoch < r.audioEpoch {
		return false
	}

	if delivery.transformed {
		if frame.AudioProvenance == avframe.FrameProvenanceSource {
			r.setAudioOwner(epoch, muxerWorkerAudioDirect)
			r.directEpochSeen = true
			return false
		}
		r.setAudioOwner(epoch, muxerWorkerAudioTransformed)
		if r.audioEpoch != epoch || r.audioOwner != muxerWorkerAudioTransformed {
			return false
		}
		if frame.FrameType == avframe.FrameTypeSequenceHeader &&
			!r.startupHeaderHandled && !r.directEpochSeen {
			r.startupHeaderHandled = true
			return false
		}
		return true
	}

	if frame.Codec == r.plan.codec {
		r.setAudioOwner(epoch, muxerWorkerAudioDirect)
		r.directEpochSeen = true
		return r.audioEpoch == epoch && r.audioOwner == muxerWorkerAudioDirect
	}
	r.setAudioOwner(epoch, muxerWorkerAudioTransformed)
	return false
}

func (r *muxerWorkerLiveInput) setAudioOwner(epoch uint64, owner muxerWorkerAudioOwner) {
	if epoch > r.audioEpoch {
		r.audioEpoch = epoch
		r.audioOwner = owner
		return
	}
	if epoch == r.audioEpoch &&
		(r.audioOwner == muxerWorkerAudioUnknown || owner == muxerWorkerAudioDirect) {
		r.audioOwner = owner
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
			reader, release, err := tm.GetOrCreateReaderAtFromHistory(avframe.CodecAAC, snapshot)
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

// waitStreamStartup waits for a stream generation to expose all of the
// sequence headers required by its declared tracks. The manager's done signal
// and the captured generation signal both cancel the wait, so a stopped or
// replaced publisher cannot leave a segmenter blocked indefinitely.
func waitStreamStartup(done <-chan struct{}, stream *core.Stream) (core.StreamStartupSnapshot, bool) {
	return waitStreamStartupForGeneration(done, stream, 0, 0, "")
}

func waitStreamStartupForPublisher(done <-chan struct{}, stream *core.Stream, expectedPublisherID string) (core.StreamStartupSnapshot, bool) {
	return waitStreamStartupForGeneration(done, stream, 0, 0, expectedPublisherID)
}

func waitStreamStartupForGeneration(done <-chan struct{}, stream *core.Stream, expectedInstanceID, expectedGeneration uint64, expectedPublisherID string) (core.StreamStartupSnapshot, bool) {
	pending := stream.StartupSnapshot()
	if pending.Generation == 0 || pending.GenerationDone == nil {
		return core.StreamStartupSnapshot{}, false
	}
	if expectedInstanceID != 0 && stream.InstanceID() != expectedInstanceID {
		return core.StreamStartupSnapshot{}, false
	}
	if expectedGeneration != 0 && pending.Generation != expectedGeneration {
		return core.StreamStartupSnapshot{}, false
	}
	if expectedPublisherID != "" && pending.PublisherID != expectedPublisherID {
		return core.StreamStartupSnapshot{}, false
	}
	if pending.Ready {
		select {
		case <-done:
			return core.StreamStartupSnapshot{}, false
		default:
		}
		return pending, stream.IsPublisherGeneration(pending.Generation) &&
			(expectedPublisherID == "" || pending.PublisherID == expectedPublisherID)
	}

	ctx, cancel := context.WithCancel(context.Background())
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-done:
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
		(expectedInstanceID == 0 || stream.InstanceID() == expectedInstanceID) &&
		(expectedGeneration == 0 || snapshot.Generation == expectedGeneration) &&
		stream.IsPublisherGeneration(snapshot.Generation) &&
		(expectedPublisherID == "" || snapshot.PublisherID == expectedPublisherID)
}

// segmentReaderContext interrupts a direct source reader when its generation
// ends so the caller can drain to the captured boundary. A transcoded reader
// stays open until its generation-bound producer drains and closes its output.
func segmentReaderContext(done, generationDone <-chan struct{}, waitForGenerationOutput bool) (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		if waitForGenerationOutput {
			select {
			case <-done:
				cancel()
			case <-ctx.Done():
			}
			return
		}
		select {
		case <-done:
			cancel()
		case <-generationDone:
			cancel()
		case <-ctx.Done():
		}
	}()
	var stopOnce sync.Once
	return ctx, func() {
		stopOnce.Do(func() {
			cancel()
			<-watcherDone
		})
	}
}

type segmentReadResult struct {
	Frame       *avframe.AVFrame
	OK          bool
	Overwritten int64
}

type segmentInputFactory func(
	stream *core.Stream,
	snapshot core.StreamStartupSnapshot,
	plan muxerAudioPlan,
) (*util.RingReader[*avframe.AVFrame], func(), muxerAudioPlan)

type segmentInputOwner struct {
	reader                  *util.RingReader[*avframe.AVFrame]
	release                 func()
	readContext             context.Context
	stopRead                func()
	waitForGenerationOutput bool
	closed                  bool
}

func newSegmentInputOwner(
	factory segmentInputFactory,
	stream *core.Stream,
	snapshot core.StreamStartupSnapshot,
	plan muxerAudioPlan,
	done <-chan struct{},
) (*segmentInputOwner, muxerAudioPlan) {
	reader, release, actualPlan := openSegmentInput(factory, stream, snapshot, plan)
	waitForGenerationOutput := actualPlan.mode == muxerAudioTranscode
	readContext, stopRead := segmentReaderContext(done, snapshot.GenerationDone, waitForGenerationOutput)
	return &segmentInputOwner{
		reader:                  reader,
		release:                 release,
		readContext:             readContext,
		stopRead:                stopRead,
		waitForGenerationOutput: waitForGenerationOutput,
	}, actualPlan
}

func (i *segmentInputOwner) Reopen(
	factory segmentInputFactory,
	stream *core.Stream,
	snapshot core.StreamStartupSnapshot,
	plan muxerAudioPlan,
	done <-chan struct{},
) muxerAudioPlan {
	i.Close()
	replacement, actualPlan := newSegmentInputOwner(factory, stream, snapshot, plan, done)
	*i = *replacement
	return actualPlan
}

func (i *segmentInputOwner) Close() {
	if i == nil || i.closed {
		return
	}
	i.closed = true
	i.stopRead()
	i.reader.Close()
	i.release()
}

func segmentInputSourceChanged(current, refreshed muxerAudioPlan) bool {
	return (current.mode == muxerAudioTranscode) != (refreshed.mode == muxerAudioTranscode)
}

func segmentRecoverySnapshot(snapshot core.StreamStartupSnapshot) core.StreamStartupSnapshot {
	snapshot.ReplayFrames = nil
	snapshot.SourceCursor = snapshot.LiveCursor
	return snapshot
}

func openSegmentInput(
	factory segmentInputFactory,
	stream *core.Stream,
	snapshot core.StreamStartupSnapshot,
	plan muxerAudioPlan,
) (*util.RingReader[*avframe.AVFrame], func(), muxerAudioPlan) {
	if factory != nil {
		return factory(stream, snapshot, plan)
	}
	return muxerLiveReaderSnapshot(stream, snapshot, plan)
}

func currentSegmentSnapshot(stream *core.Stream, original core.StreamStartupSnapshot) (core.StreamStartupSnapshot, bool) {
	current := stream.StartupSnapshot()
	if !current.Ready || current.StreamInstanceID != original.StreamInstanceID ||
		current.Generation != original.Generation || !stream.IsPublisherGeneration(original.Generation) {
		return core.StreamStartupSnapshot{}, false
	}
	return current, true
}

func logSegmentOverwrite(format, action string, overwritten int64) {
	slog.Warn("segmenter input continuity lost",
		"module", "httpstream",
		"format", format,
		"consumer", "segmenter",
		"action", action,
		"overwritten", overwritten,
	)
}

func logSegmentProducerEnd(format string) {
	slog.Warn("segmenter input ended while publisher generation remained active",
		"module", "httpstream",
		"format", format,
		"consumer", "segmenter",
		"action", "producer_end",
	)
}

func readSegmentFrame(
	ctx context.Context,
	reader *util.RingReader[*avframe.AVFrame],
	snapshot core.StreamStartupSnapshot,
	waitForGenerationOutput bool,
) segmentReadResult {
	for {
		if !waitForGenerationOutput {
			if endCursor, ended := snapshot.GenerationEndCursor(); ended {
				if reader.ReadCursor() >= endCursor {
					return segmentReadResult{}
				}
				result := reader.TryReadResult()
				if !result.OK {
					return segmentReadResult{}
				}
				if result.Overwritten == 0 && reader.ReadCursor() > endCursor {
					return segmentReadResult{}
				}
				return segmentReadResult{
					Frame:       result.Value,
					OK:          result.OK,
					Overwritten: result.Overwritten,
				}
			}
		}

		result := reader.ReadResultContext(ctx)
		if result.OK {
			return segmentReadResult{
				Frame:       result.Value,
				OK:          result.OK,
				Overwritten: result.Overwritten,
			}
		}
		if !waitForGenerationOutput {
			if _, ended := snapshot.GenerationEndCursor(); ended {
				continue
			}
		}
		return segmentReadResult{}
	}
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
