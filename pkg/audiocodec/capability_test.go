//go:build audiocodec

package audiocodec

import (
	"testing"

	"github.com/im-pingo/liveforge/pkg/avframe"
)

func TestFullBuildAdvertisesAllConfiguredAudioCodecs(t *testing.T) {
	for _, codec := range []avframe.CodecType{
		avframe.CodecAAC,
		avframe.CodecOpus,
		avframe.CodecMP3,
		avframe.CodecG711U,
		avframe.CodecG711A,
		avframe.CodecG722,
		avframe.CodecSpeex,
	} {
		decoder, err := Global().NewDecoder(codec)
		if err != nil {
			t.Errorf("decoder for %s is not registered: %v", codec, err)
		} else {
			decoder.Close()
		}
		encoder, err := Global().NewEncoder(codec)
		if err != nil {
			t.Errorf("encoder for %s is not registered: %v", codec, err)
		} else {
			encoder.Close()
		}
	}
	resampler := Global().NewResampler(48000, 2, 44100, 2)
	if resampler == nil {
		t.Fatal("audio resampler is not registered")
	}
	resampler.Close()
	if Global().SequenceHeader(avframe.CodecAAC) == nil {
		t.Fatal("AAC sequence header is not registered")
	}
}
