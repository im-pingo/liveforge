package httpstream

import (
	"bytes"
	"log"

	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/codec/aac"
	"github.com/im-pingo/liveforge/pkg/muxer/flv"
	"github.com/im-pingo/liveforge/pkg/muxer/fmp4"
	"github.com/im-pingo/liveforge/pkg/muxer/ts"
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
	log.Printf("[httpstream] FLV muxer started for %s", stream.Key())

	muxer := flv.NewMuxer()
	var buf bytes.Buffer

	// Write FLV header as init data
	hasVideo := stream.VideoSeqHeader() != nil
	hasAudio := stream.AudioSeqHeader() != nil
	muxer.WriteHeader(&buf, hasVideo, hasAudio)

	// Write sequence headers into the FLV stream
	if vsh := stream.VideoSeqHeader(); vsh != nil {
		muxer.WriteFrame(&buf, vsh)
	}
	if ash := stream.AudioSeqHeader(); ash != nil {
		muxer.WriteFrame(&buf, ash)
	}

	inst.SetInitData(bufCopyAndReset(&buf))

	// Snapshot write cursor before sending GOP cache, so the reader
	// starts right after the cached frames and we avoid duplicates.
	startPos := stream.RingBuffer().WriteCursor()

	// Send GOP cache
	for _, f := range stream.GOPCache() {
		if err := muxer.WriteFrame(&buf, f); err == nil && buf.Len() > 0 {
			inst.Buffer.Write(bufCopyAndReset(&buf))
		}
	}

	// Read live frames from ring buffer (only new frames)
	reader := stream.RingBuffer().NewReaderAt(startPos)
	for {
		select {
		case <-inst.Done:
			log.Printf("[httpstream] FLV muxer stopped for %s", stream.Key())
			return
		default:
		}

		frame, ok := reader.Read()
		if !ok || frame == nil {
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
	log.Printf("[httpstream] TS muxer started for %s", stream.Key())

	// Determine codecs from sequence headers
	var videoCodec, audioCodec avframe.CodecType
	var videoSeqData, audioSeqData []byte

	if vsh := stream.VideoSeqHeader(); vsh != nil {
		videoCodec = vsh.Codec
		videoSeqData = vsh.Payload
	}
	if ash := stream.AudioSeqHeader(); ash != nil {
		audioCodec = ash.Codec
		audioSeqData = ash.Payload
	}

	muxer := ts.NewMuxer(videoCodec, audioCodec, videoSeqData, audioSeqData)

	// No init data needed for TS (PAT/PMT sent inline)

	// Snapshot write cursor before sending GOP cache
	startPos := stream.RingBuffer().WriteCursor()

	// Send GOP cache
	for _, f := range stream.GOPCache() {
		if data := muxer.WriteFrame(f); len(data) > 0 {
			inst.Buffer.Write(copyBytes(data))
		}
	}

	// Read live frames (only new frames)
	reader := stream.RingBuffer().NewReaderAt(startPos)
	for {
		select {
		case <-inst.Done:
			log.Printf("[httpstream] TS muxer stopped for %s", stream.Key())
			return
		default:
		}

		frame, ok := reader.Read()
		if !ok || frame == nil {
			return
		}

		if data := muxer.WriteFrame(frame); len(data) > 0 {
			inst.Buffer.Write(copyBytes(data))
		}
	}
}

func (m *Module) runFMP4Muxer(inst *core.MuxerInstance, stream *core.Stream) {
	defer inst.Buffer.Close()
	log.Printf("[httpstream] FMP4 muxer started for %s", stream.Key())

	var videoCodec, audioCodec avframe.CodecType
	var videoSeqHeader, audioSeqHeader *avframe.AVFrame

	if vsh := stream.VideoSeqHeader(); vsh != nil {
		videoCodec = vsh.Codec
		videoSeqHeader = vsh
	}
	if ash := stream.AudioSeqHeader(); ash != nil {
		audioCodec = ash.Codec
		audioSeqHeader = ash
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

	// For FMP4, we buffer a GOP and emit as a segment
	var gopBuf []*avframe.AVFrame

	// Snapshot write cursor before sending GOP cache
	startPos := stream.RingBuffer().WriteCursor()

	// Send GOP cache as first segment
	gopCache := stream.GOPCache()
	if len(gopCache) > 0 {
		seg := muxer.WriteSegment(gopCache)
		if len(seg) > 0 {
			inst.Buffer.Write(seg)
		}
	}

	// Read live frames, buffer by GOP (only new frames)
	reader := stream.RingBuffer().NewReaderAt(startPos)
	for {
		select {
		case <-inst.Done:
			log.Printf("[httpstream] FMP4 muxer stopped for %s", stream.Key())
			return
		default:
		}

		frame, ok := reader.Read()
		if !ok || frame == nil {
			return
		}
		if frame.FrameType == avframe.FrameTypeSequenceHeader {
			continue
		}

		// On video keyframe, flush previous GOP as a segment
		if frame.MediaType.IsVideo() && frame.FrameType.IsKeyframe() && len(gopBuf) > 0 {
			seg := muxer.WriteSegment(gopBuf)
			if len(seg) > 0 {
				inst.Buffer.Write(seg)
			}
			gopBuf = gopBuf[:0]
		}

		gopBuf = append(gopBuf, frame)
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
