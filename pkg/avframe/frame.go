package avframe

// MediaType distinguishes audio and video frames.
type MediaType uint8

const (
	MediaTypeVideo MediaType = iota + 1
	MediaTypeAudio
)

func (m MediaType) IsVideo() bool { return m == MediaTypeVideo }
func (m MediaType) IsAudio() bool { return m == MediaTypeAudio }

// CodecType identifies the codec of a frame.
type CodecType uint8

const (
	// Video codecs (1-49)
	CodecH264 CodecType = iota + 1
	CodecH265
	CodecAV1
	CodecVP8
	CodecVP9
)

const (
	// Audio codecs (50+)
	CodecAAC  CodecType = iota + 50
	CodecOpus
	CodecMP3
	CodecG711A // PCMA
	CodecG711U // PCMU
	CodecG722
	CodecG729
	CodecSpeex
)

var codecNames = map[CodecType]string{
	CodecH264: "H264", CodecH265: "H265", CodecAV1: "AV1",
	CodecVP8: "VP8", CodecVP9: "VP9",
	CodecAAC: "AAC", CodecOpus: "Opus", CodecMP3: "MP3",
	CodecG711A: "G711A", CodecG711U: "G711U",
	CodecG722: "G722", CodecG729: "G729", CodecSpeex: "Speex",
}

func (c CodecType) String() string {
	if s, ok := codecNames[c]; ok {
		return s
	}
	return "Unknown"
}

func (c CodecType) IsVideo() bool {
	return c >= CodecH264 && c <= CodecVP9
}

func (c CodecType) IsAudio() bool {
	return c >= CodecAAC && c <= CodecSpeex
}

// FrameType distinguishes keyframes from inter-frames.
type FrameType uint8

const (
	FrameTypeKeyframe       FrameType = iota + 1
	FrameTypeInterframe
	FrameTypeSequenceHeader // SPS/PPS, AudioSpecificConfig, etc.
)

func (f FrameType) IsKeyframe() bool { return f == FrameTypeKeyframe }

// AVFrame is the universal internal media frame.
type AVFrame struct {
	MediaType MediaType
	Codec     CodecType
	FrameType FrameType
	DTS       int64  // Decode timestamp in milliseconds
	PTS       int64  // Presentation timestamp in milliseconds
	Payload   []byte // Raw codec data (no container framing)
}

// NewAVFrame creates a new AVFrame.
func NewAVFrame(mediaType MediaType, codec CodecType, frameType FrameType, dts, pts int64, payload []byte) *AVFrame {
	return &AVFrame{
		MediaType: mediaType,
		Codec:     codec,
		FrameType: frameType,
		DTS:       dts,
		PTS:       pts,
		Payload:   payload,
	}
}
