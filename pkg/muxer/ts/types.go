package ts

import "github.com/im-pingo/liveforge/pkg/avframe"

const (
	PacketSize     = 188
	SyncByte       = 0x47
	PIDPat         = 0x0000
	PIDPmt         = 0x1000
	PIDVideo       = 0x0100
	PIDAudio       = 0x0101
	PIDPCR         = PIDVideo
	MaxPCRInterval = 40 // milliseconds
)

// StreamType returns the MPEG-TS stream_type for a given codec.
func StreamType(codec avframe.CodecType) byte {
	switch codec {
	case avframe.CodecH264:
		return 0x1B // AVC video
	case avframe.CodecH265:
		return 0x24 // HEVC video
	case avframe.CodecAV1:
		return 0x06 // private data (registration descriptor used)
	case avframe.CodecAAC:
		return 0x0F // AAC ADTS
	case avframe.CodecMP3:
		return 0x03 // MPEG-1 audio
	case avframe.CodecOpus:
		return 0x06 // private data
	default:
		return 0
	}
}

// MPEG-2 CRC32 table (polynomial 0x04C11DB7).
var crc32Table [256]uint32

func init() {
	for i := 0; i < 256; i++ {
		crc := uint32(i) << 24
		for j := 0; j < 8; j++ {
			if crc&0x80000000 != 0 {
				crc = (crc << 1) ^ 0x04C11DB7
			} else {
				crc <<= 1
			}
		}
		crc32Table[i] = crc
	}
}

// CRC32MPEG2 computes the MPEG-2 CRC32.
func CRC32MPEG2(data []byte) uint32 {
	crc := uint32(0xFFFFFFFF)
	for _, b := range data {
		crc = (crc << 8) ^ crc32Table[(crc>>24)^uint32(b)]
	}
	return crc
}
