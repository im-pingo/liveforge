package source

import (
	"io"

	"github.com/im-pingo/liveforge/pkg/avframe"
)

// MediaInfo describes the codecs discovered in a media source.
type MediaInfo struct {
	VideoCodec avframe.CodecType
	AudioCodec avframe.CodecType
}

// HasVideo reports whether the source contains a video track.
func (m *MediaInfo) HasVideo() bool { return m.VideoCodec != 0 }

// HasAudio reports whether the source contains an audio track.
func (m *MediaInfo) HasAudio() bool { return m.AudioCodec != 0 }

// Source provides deterministic media frames for testing.
// Implementations must be safe for sequential use but need not be goroutine-safe.
type Source interface {
	// NextFrame returns the next AVFrame or io.EOF when done.
	NextFrame() (*avframe.AVFrame, error)

	// MediaInfo returns codec information discovered during the initial scan.
	// It is available immediately after construction.
	MediaInfo() *MediaInfo

	// Reset rewinds the source to the beginning.
	Reset()
}

// Ensure io is used (EOF is the end-of-stream signal for NextFrame).
var _ error = io.EOF
