//go:build audiocodec

package push

import (
	"fmt"

	"github.com/im-pingo/liveforge/pkg/audiocodec"
	"github.com/im-pingo/liveforge/pkg/avframe"
)

type whipAudioTranscoder struct {
	decoder   audiocodec.Decoder
	encoder   audiocodec.Encoder
	resampler audiocodec.Resampler
	pcm       []int16
}

func newWHIPAudioTranscoder(codec avframe.CodecType) (whipAudioProcessor, error) {
	registry := audiocodec.Global()
	decoder, err := registry.NewDecoder(codec)
	if err != nil {
		return nil, fmt.Errorf("whip: audio decoder for %s: %w", codec, err)
	}
	encoder, err := registry.NewEncoder(avframe.CodecOpus)
	if err != nil {
		decoder.Close()
		return nil, fmt.Errorf("whip: Opus encoder: %w", err)
	}
	return &whipAudioTranscoder{decoder: decoder, encoder: encoder}, nil
}

func (p *whipAudioTranscoder) Process(frame *avframe.AVFrame) ([][]byte, error) {
	if frame == nil || !frame.MediaType.IsAudio() {
		return nil, nil
	}
	if frame.FrameType == avframe.FrameTypeSequenceHeader {
		p.decoder.SetExtradata(frame.Payload)
		return nil, nil
	}
	pcm, err := p.decoder.Decode(frame.Payload)
	if err != nil {
		return nil, err
	}
	if p.resampler == nil {
		p.resampler = audiocodec.Global().NewResampler(pcm.SampleRate, pcm.Channels, 48000, 2)
		if p.resampler == nil {
			return nil, fmt.Errorf("48 kHz stereo resampler unavailable")
		}
	}
	converted := p.resampler.Resample(pcm)
	p.pcm = append(p.pcm, converted.Samples...)

	frameSize := p.encoder.FrameSize()
	if frameSize <= 0 {
		return nil, fmt.Errorf("Opus encoder returned invalid frame size %d", frameSize)
	}
	needed := frameSize * 2
	packets := make([][]byte, 0, len(p.pcm)/needed)
	for len(p.pcm) >= needed {
		payload, encodeErr := p.encoder.Encode(&audiocodec.PCMFrame{
			Samples:    p.pcm[:needed],
			SampleRate: 48000,
			Channels:   2,
		})
		if encodeErr != nil {
			return nil, encodeErr
		}
		copy(p.pcm, p.pcm[needed:])
		p.pcm = p.pcm[:len(p.pcm)-needed]
		if len(payload) > 0 {
			packets = append(packets, payload)
		}
	}
	return packets, nil
}

func (p *whipAudioTranscoder) Close() {
	if p.resampler != nil {
		p.resampler.Close()
	}
	if p.encoder != nil {
		if drainer, ok := p.encoder.(audiocodec.DrainingEncoder); ok {
			_, _ = drainer.Drain()
		}
		p.encoder.Close()
	}
	if p.decoder != nil {
		p.decoder.Close()
	}
}
