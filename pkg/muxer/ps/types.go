// Package ps implements MPEG-PS (Program Stream) muxer and demuxer
// for GB28181 video surveillance transport.
package ps

import "github.com/im-pingo/liveforge/pkg/avframe"

// PS start codes (ISO 13818-1).
const (
	PackHeaderStartCode       = 0x000001BA
	SystemHeaderStartCode     = 0x000001BB
	ProgramStreamMapStartCode = 0x000001BC
	PESVideoStreamID          = 0xE0
	PESAudioStreamID          = 0xC0
	PESPrivateStream1ID       = 0xBD
)

// StreamType returns the MPEG-PS stream_type for a given codec.
func StreamType(codec avframe.CodecType) byte {
	switch codec {
	case avframe.CodecH264:
		return 0x1B
	case avframe.CodecH265:
		return 0x24
	case avframe.CodecAAC:
		return 0x0F
	case avframe.CodecG711A:
		return 0x90
	case avframe.CodecG711U:
		return 0x91
	default:
		return 0
	}
}

// CodecFromStreamType maps an MPEG-PS stream_type back to a CodecType.
func CodecFromStreamType(st byte) avframe.CodecType {
	switch st {
	case 0x1B:
		return avframe.CodecH264
	case 0x24:
		return avframe.CodecH265
	case 0x0F:
		return avframe.CodecAAC
	case 0x90:
		return avframe.CodecG711A
	case 0x91:
		return avframe.CodecG711U
	default:
		return 0
	}
}
