package httpstream

import (
	"bytes"
	"log"

	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/muxer/flv"
	"github.com/im-pingo/liveforge/pkg/muxer/fmp4"
	"github.com/im-pingo/liveforge/pkg/muxer/ts"
)

// ensureMuxerCallbacks registers muxer start callbacks for a stream (idempotent).
func (m *Module) ensureMuxerCallbacks(stream *core.Stream) {
	m.registeredMu.Lock()
	defer m.registeredMu.Unlock()

	key := stream.Key()
	if m.registered[key] {
		return
	}
	m.registered[key] = true

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

	initBytes := make([]byte, buf.Len())
	copy(initBytes, buf.Bytes())
	inst.SetInitData(initBytes)
	buf.Reset()

	// Send GOP cache
	for _, f := range stream.GOPCache() {
		if err := muxer.WriteFrame(&buf, f); err == nil && buf.Len() > 0 {
			inst.Buffer.Write(buf.Bytes())
			buf.Reset()
		}
	}

	// Read live frames from ring buffer
	reader := stream.RingBuffer().NewReader()
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
			inst.Buffer.Write(buf.Bytes())
			buf.Reset()
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

	// Send GOP cache
	for _, f := range stream.GOPCache() {
		if data := muxer.WriteFrame(f); len(data) > 0 {
			inst.Buffer.Write(data)
		}
	}

	// Read live frames
	reader := stream.RingBuffer().NewReader()
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
			inst.Buffer.Write(data)
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

	// Build init segment
	// TODO: extract width/height/sampleRate/channels from sequence headers
	initSeg := muxer.Init(videoSeqHeader, audioSeqHeader, 0, 0, 44100, 2)
	inst.SetInitData(initSeg)

	// For FMP4, we buffer a GOP and emit as a segment
	var gopBuf []*avframe.AVFrame

	// Send GOP cache as first segment
	gopCache := stream.GOPCache()
	if len(gopCache) > 0 {
		seg := muxer.WriteSegment(gopCache)
		if len(seg) > 0 {
			inst.Buffer.Write(seg)
		}
	}

	// Read live frames, buffer by GOP
	reader := stream.RingBuffer().NewReader()
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
