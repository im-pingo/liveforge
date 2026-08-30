//go:build !audiocodec

package push

import "github.com/im-pingo/liveforge/pkg/avframe"

func newWHIPAudioTranscoder(avframe.CodecType) (whipAudioProcessor, error) {
	return nil, nil
}
