package httpstream

import (
	"bytes"
	"log/slog"

	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/audiocodec"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/codec/aac"
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

	// Check if audio transcoding is needed for FLV (supports AAC/MP3 only).
	var sourceAudioCodec avframe.CodecType
	if pub := stream.Publisher(); pub != nil && pub.MediaInfo() != nil {
		sourceAudioCodec = pub.MediaInfo().AudioCodec
	}
	audioCompatible := sourceAudioCodec == 0 || isFlvCompatibleAudio(sourceAudioCodec)

	muxer := flv.NewMuxer()
	var buf bytes.Buffer

	// Write FLV header as init data
	hasVideo := stream.VideoSeqHeader() != nil
	hasAudio := stream.AudioSeqHeader() != nil || !audioCompatible
	muxer.WriteHeader(&buf, hasVideo, hasAudio)

	// Write sequence headers into the FLV stream
	if vsh := stream.VideoSeqHeader(); vsh != nil {
		muxer.WriteFrame(&buf, vsh)
	}
	if audioCompatible {
		if ash := stream.AudioSeqHeader(); ash != nil {
			muxer.WriteFrame(&buf, ash)
		}
	} else {
		// Transcoding to AAC: use synthetic sequence header
		if ash := aacSeqHeaderFrame(); ash != nil {
			muxer.WriteFrame(&buf, ash)
		}
	}

	inst.SetInitData(bufCopyAndReset(&buf))

	// Snapshot write cursor before sending GOP cache, so the reader
	// starts right after the cached frames and we avoid duplicates.
	startPos := stream.RingBuffer().WriteCursor()

	// Send GOP cache (skip audio if transcoding)
	for _, f := range stream.GOPCache() {
		if !audioCompatible && f.MediaType.IsAudio() {
			continue
		}
		if err := muxer.WriteFrame(&buf, f); err == nil && buf.Len() > 0 {
			inst.Buffer.Write(bufCopyAndReset(&buf))
		}
	}

	// Read live frames
	reader, release := muxerLiveReader(stream, startPos, audioCompatible)
	defer release()

	// Close reader when muxer is done so Read() unblocks promptly.
	go func() {
		<-inst.Done
		reader.Close()
	}()

	for {
		frame, ok := reader.Read()
		if !ok || frame == nil {
			slog.Info("muxer stopped", "module", "httpstream", "format", "flv", "stream", stream.Key())
			return
		}
		if frame.FrameType == avframe.FrameTypeSequenceHeader {
			continue // Already sent in init data
		}

		if err := muxer.WriteFrame(&buf, frame); err == nil && buf.Len() > 0 {
			inst.Buffer.Write(bufCopyAndReset(&buf))
		}
	}
}

func (m *Module) runTSMuxer(inst *core.MuxerInstance, stream *core.Stream) {
	defer inst.Buffer.Close()
	slog.Info("muxer started", "module", "httpstream", "format", "ts", "stream", stream.Key())

	// Check if audio transcoding is needed for TS (supports AAC/MP3 only).
	var sourceAudioCodec avframe.CodecType
	if pub := stream.Publisher(); pub != nil && pub.MediaInfo() != nil {
		sourceAudioCodec = pub.MediaInfo().AudioCodec
	}
	audioCompatible := sourceAudioCodec == 0 || isFlvCompatibleAudio(sourceAudioCodec)

	// Determine codecs from sequence headers
	var videoCodec, audioCodec avframe.CodecType
	var videoSeqData, audioSeqData []byte

	if vsh := stream.VideoSeqHeader(); vsh != nil {
		videoCodec = vsh.Codec
		videoSeqData = vsh.Payload
	}
	if audioCompatible {
		if ash := stream.AudioSeqHeader(); ash != nil {
			audioCodec = ash.Codec
			audioSeqData = ash.Payload
		}
	} else {
		// Transcoding to AAC
		audioCodec = avframe.CodecAAC
		if seqHdr := audiocodec.Global().SequenceHeader(avframe.CodecAAC); seqHdr != nil {
			audioSeqData = seqHdr
		}
	}

	muxer := ts.NewMuxer(videoCodec, audioCodec, videoSeqData, audioSeqData)

	// No init data needed for TS (PAT/PMT sent inline)

	// Snapshot write cursor before sending GOP cache
	startPos := stream.RingBuffer().WriteCursor()

	// Send GOP cache (skip audio if transcoding)
	for _, f := range stream.GOPCache() {
		if !audioCompatible && f.MediaType.IsAudio() {
			continue
		}
		if data := muxer.WriteFrame(f); len(data) > 0 {
			inst.Buffer.Write(data)
		}
	}

	// Read live frames
	reader, release := muxerLiveReader(stream, startPos, audioCompatible)
	defer release()

	// Close reader when muxer is done so Read() unblocks promptly.
	go func() {
		<-inst.Done
		reader.Close()
	}()

	for {
		frame, ok := reader.Read()
		if !ok || frame == nil {
			slog.Info("muxer stopped", "module", "httpstream", "format", "ts", "stream", stream.Key())
			return
		}

		if data := muxer.WriteFrame(frame); len(data) > 0 {
			inst.Buffer.Write(data)
		}
	}
}

func (m *Module) runFMP4Muxer(inst *core.MuxerInstance, stream *core.Stream) {
	defer inst.Buffer.Close()
	slog.Info("muxer started", "module", "httpstream", "format", "fmp4", "stream", stream.Key())

	// Check if audio transcoding is needed for fMP4 (supports AAC/MP3/Opus).
	var sourceAudioCodec avframe.CodecType
	if pub := stream.Publisher(); pub != nil && pub.MediaInfo() != nil {
		sourceAudioCodec = pub.MediaInfo().AudioCodec
	}
	audioCompatible := sourceAudioCodec == 0 ||
		sourceAudioCodec == avframe.CodecAAC ||
		sourceAudioCodec == avframe.CodecMP3 ||
		sourceAudioCodec == avframe.CodecOpus

	var videoCodec, audioCodec avframe.CodecType
	var videoSeqHeader, audioSeqHeader *avframe.AVFrame

	if vsh := stream.VideoSeqHeader(); vsh != nil {
		videoCodec = vsh.Codec
		videoSeqHeader = vsh
	}
	if audioCompatible {
		if ash := stream.AudioSeqHeader(); ash != nil {
			audioCodec = ash.Codec
			audioSeqHeader = ash
		}
	} else {
		// Transcoding to AAC
		audioCodec = avframe.CodecAAC
		audioSeqHeader = aacSeqHeaderFrame()
	}

	muxer := fmp4.NewMuxer(videoCodec, audioCodec)

	// Extract video dimensions from the AVC sequence header (AVCDecoderConfigurationRecord).
	// Chrome requires non-zero coded_size in the fMP4 init segment.
	var videoWidth, videoHeight int
	if videoSeqHeader != nil && videoCodec == avframe.CodecH264 {
		videoWidth, videoHeight = fmp4.ParseAVCCDimensions(videoSeqHeader.Payload)
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

	// Snapshot write cursor before sending GOP cache
	startPos := stream.RingBuffer().WriteCursor()

	// Send GOP cache as first segment (skip audio if transcoding)
	gopCache := stream.GOPCache()
	if len(gopCache) > 0 {
		if !audioCompatible {
			var filtered []*avframe.AVFrame
			for _, f := range gopCache {
				if !f.MediaType.IsAudio() {
					filtered = append(filtered, f)
				}
			}
			gopCache = filtered
		}
		seg := muxer.WriteSegment(gopCache)
		if len(seg) > 0 {
			inst.Buffer.Write(seg)
		}
	}

	// Read live frames and emit each as its own moof+mdat segment.
	reader, release := muxerLiveReader(stream, startPos, audioCompatible)
	defer release()

	// Close reader when muxer is done so Read() unblocks promptly.
	go func() {
		<-inst.Done
		reader.Close()
	}()

	for {
		frame, ok := reader.Read()
		if !ok || frame == nil {
			slog.Info("muxer stopped", "module", "httpstream", "format", "fmp4", "stream", stream.Key())
			return
		}
		if frame.FrameType == avframe.FrameTypeSequenceHeader {
			continue
		}

		seg := muxer.WriteSegment([]*avframe.AVFrame{frame})
		if len(seg) > 0 {
			inst.Buffer.Write(seg)
		}
	}
}

// parseAudioSeqHeader extracts sample rate and channel count from an AAC
// audio sequence header frame. Returns (0, 0) on error.
func parseAudioSeqHeader(frame *avframe.AVFrame) (sampleRate, channels int) {
	info, err := aac.ParseAudioSpecificConfig(frame.Payload)
	if err != nil {
		return 0, 0
	}
	return info.SampleRate, info.Channels
}

// isFlvCompatibleAudio returns true if the codec is FLV/TS container-compatible.
func isFlvCompatibleAudio(codec avframe.CodecType) bool {
	return codec == avframe.CodecAAC || codec == avframe.CodecMP3
}

// muxerLiveReader returns a ring reader for live frames. If the publisher's
// audio codec is not compatible with the target container (FLV/TS/fMP4),
// it returns a TranscodeManager reader that transcodes audio to AAC.
// The caller must call the returned release function when done.
func muxerLiveReader(stream *core.Stream, startPos int64, compatible bool) (*util.RingReader[*avframe.AVFrame], func()) {
	if !compatible {
		if tm := stream.TranscodeManager(); tm != nil {
			reader, release, err := tm.GetOrCreateReader(avframe.CodecAAC)
			if err == nil {
				return reader, release
			}
			slog.Warn("muxer: audio transcode unavailable", "stream", stream.Key(), "error", err)
		}
	}
	return stream.RingBuffer().NewReaderAt(startPos), func() {}
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
