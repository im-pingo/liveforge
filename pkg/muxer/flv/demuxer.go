package flv

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/im-pingo/liveforge/pkg/avframe"
)

// Demuxer reads FLV tags from a byte stream and converts them to AVFrames.
type Demuxer struct {
	r          io.Reader
	headerRead bool
}

// NewDemuxer creates a new FLV demuxer.
func NewDemuxer(r io.Reader) *Demuxer {
	return &Demuxer{r: r}
}

// ReadTag reads the next FLV tag and returns an AVFrame.
// Skips script data tags. Returns io.EOF when no more data.
func (d *Demuxer) ReadTag() (*avframe.AVFrame, error) {
	if !d.headerRead {
		if err := d.readHeader(); err != nil {
			return nil, fmt.Errorf("read FLV header: %w", err)
		}
		d.headerRead = true
	}

	for {
		frame, err := d.readOneTag()
		if err != nil {
			return nil, err
		}
		if frame != nil {
			return frame, nil
		}
		// frame == nil means we skipped a script tag, continue reading
	}
}

func (d *Demuxer) readHeader() error {
	// FLV header: 9 bytes + 4 bytes PreviousTagSize0
	header := make([]byte, 9+4)
	if _, err := io.ReadFull(d.r, header); err != nil {
		return err
	}
	if header[0] != 'F' || header[1] != 'L' || header[2] != 'V' {
		return fmt.Errorf("invalid FLV signature: %x %x %x", header[0], header[1], header[2])
	}
	return nil
}

func (d *Demuxer) readOneTag() (*avframe.AVFrame, error) {
	// Tag header: 11 bytes
	var tagHeader [TagHeaderSize]byte
	if _, err := io.ReadFull(d.r, tagHeader[:]); err != nil {
		return nil, err
	}

	tagType := tagHeader[0]
	dataSize := int(tagHeader[1])<<16 | int(tagHeader[2])<<8 | int(tagHeader[3])
	timestamp := int64(tagHeader[4])<<16 | int64(tagHeader[5])<<8 | int64(tagHeader[6]) | int64(tagHeader[7])<<24

	// Read tag data
	data := make([]byte, dataSize)
	if _, err := io.ReadFull(d.r, data); err != nil {
		return nil, fmt.Errorf("read tag data: %w", err)
	}

	// Read previous tag size (4 bytes)
	var prevTagSize [4]byte
	if _, err := io.ReadFull(d.r, prevTagSize[:]); err != nil {
		return nil, fmt.Errorf("read previous tag size: %w", err)
	}

	switch tagType {
	case TagTypeVideo:
		return d.parseVideoTag(data, timestamp)
	case TagTypeAudio:
		return d.parseAudioTag(data, timestamp)
	case TagTypeScript:
		// Skip script data
		return nil, nil
	default:
		return nil, fmt.Errorf("unknown FLV tag type: %d", tagType)
	}
}

func (d *Demuxer) parseVideoTag(data []byte, dts int64) (*avframe.AVFrame, error) {
	if len(data) < 5 {
		return nil, fmt.Errorf("video tag data too short: %d bytes", len(data))
	}

	frameTypeID := (data[0] >> 4) & 0x0F
	codecID := data[0] & 0x0F
	avcPacketType := data[1]
	cts := int64(int32(binary.BigEndian.Uint32([]byte{0, data[2], data[3], data[4]})) >> 8)

	codec := FLVVideoCodecToAVFrame(codecID)
	if codec == 0 {
		return nil, fmt.Errorf("unsupported video codec ID: %d", codecID)
	}

	var frameType avframe.FrameType
	if avcPacketType == AVCPacketSequenceHeader {
		frameType = avframe.FrameTypeSequenceHeader
	} else if frameTypeID == VideoFrameKeyframe {
		frameType = avframe.FrameTypeKeyframe
	} else {
		frameType = avframe.FrameTypeInterframe
	}

	pts := dts + cts
	payload := data[5:]

	return avframe.NewAVFrame(avframe.MediaTypeVideo, codec, frameType, dts, pts, payload), nil
}

func (d *Demuxer) parseAudioTag(data []byte, dts int64) (*avframe.AVFrame, error) {
	if len(data) < 2 {
		return nil, fmt.Errorf("audio tag data too short: %d bytes", len(data))
	}

	formatID := (data[0] >> 4) & 0x0F
	codec := FLVAudioCodecToAVFrame(formatID)
	if codec == 0 {
		return nil, fmt.Errorf("unsupported audio format ID: %d", formatID)
	}

	var frameType avframe.FrameType
	if codec == avframe.CodecAAC && data[1] == AACPacketSequenceHeader {
		frameType = avframe.FrameTypeSequenceHeader
	} else {
		frameType = avframe.FrameTypeInterframe
	}

	payload := data[2:]

	return avframe.NewAVFrame(avframe.MediaTypeAudio, codec, frameType, dts, dts, payload), nil
}
