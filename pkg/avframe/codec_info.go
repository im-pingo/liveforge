package avframe

// MediaInfo describes the codec configuration of a stream.
type MediaInfo struct {
	VideoCodec          CodecType
	AudioCodec          CodecType
	Width               int
	Height              int
	VideoSequenceHeader []byte // SPS/PPS for H.264, VPS/SPS/PPS for H.265
	AudioSequenceHeader []byte // AudioSpecificConfig for AAC, etc.
	SampleRate          int
	Channels            int
}

// HasVideo returns true if the stream has a video track.
func (m *MediaInfo) HasVideo() bool { return m.VideoCodec != 0 }

// HasAudio returns true if the stream has an audio track.
func (m *MediaInfo) HasAudio() bool { return m.AudioCodec != 0 }
