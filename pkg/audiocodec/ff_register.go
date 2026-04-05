package audiocodec

import "github.com/im-pingo/liveforge/pkg/avframe"

// codecTable maps each supported CodecType to the FFmpeg decoder/encoder
// names and default sample-rate / channel-count used when opening them.
var codecTable = []struct {
	typ     avframe.CodecType
	decName string
	encName string
	rate    int
	ch      int
}{
	{avframe.CodecAAC, "aac", "aac", 44100, 2},
	{avframe.CodecOpus, "libopus", "libopus", 48000, 2},
	{avframe.CodecMP3, "mp3float", "libmp3lame", 44100, 2},
	{avframe.CodecG711U, "pcm_mulaw", "pcm_mulaw", 8000, 1},
	{avframe.CodecG711A, "pcm_alaw", "pcm_alaw", 8000, 1},
	{avframe.CodecG722, "g722", "g722", 16000, 1},
	{avframe.CodecSpeex, "libspeex", "libspeex", 8000, 1},
}

func init() {
	r := Global()
	for _, c := range codecTable {
		r.RegisterDecoder(c.typ, func() Decoder {
			return NewFFmpegDecoder(c.decName)
		})
		r.RegisterEncoder(c.typ, func() Encoder {
			return NewFFmpegEncoder(c.encName, c.rate, c.ch)
		})
	}

	r.RegisterSequenceHeader(avframe.CodecAAC, func() SequenceHeaderFunc {
		return aacSequenceHeader
	})
}

// aacSequenceHeader returns a minimal AAC AudioSpecificConfig for
// AAC-LC at 44100 Hz stereo (profile=2, freq_index=4, ch_config=2).
func aacSequenceHeader() []byte {
	return []byte{0x12, 0x10}
}
