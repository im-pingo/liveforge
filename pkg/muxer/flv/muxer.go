package flv

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/im-pingo/liveforge/pkg/avframe"
)

// Muxer packs AVFrames into FLV tags.
type Muxer struct{}

// NewMuxer creates a new FLV muxer.
func NewMuxer() *Muxer {
	return &Muxer{}
}

// WriteHeader writes the FLV file header.
func (m *Muxer) WriteHeader(w io.Writer, hasVideo, hasAudio bool) error {
	header := make([]byte, 9)
	copy(header, []byte("FLV"))
	header[3] = 0x01 // version
	var flags byte
	if hasAudio {
		flags |= 0x04
	}
	if hasVideo {
		flags |= 0x01
	}
	header[4] = flags
	binary.BigEndian.PutUint32(header[5:9], 9) // header size

	if _, err := w.Write(header); err != nil {
		return err
	}
	// PreviousTagSize0
	_, err := w.Write(PreviousTagSize0)
	return err
}

// WriteFrame writes an AVFrame as an FLV tag.
func (m *Muxer) WriteFrame(w io.Writer, frame *avframe.AVFrame) error {
	if frame.MediaType.IsVideo() {
		return m.writeVideoTag(w, frame)
	}
	if frame.MediaType.IsAudio() {
		return m.writeAudioTag(w, frame)
	}
	return fmt.Errorf("unsupported media type: %v", frame.MediaType)
}

func (m *Muxer) writeVideoTag(w io.Writer, frame *avframe.AVFrame) error {
	codecID := VideoCodecToFLV(frame.Codec)
	if codecID == 0 {
		return fmt.Errorf("unsupported video codec: %v", frame.Codec)
	}

	frameTypeID := AVFrameTypeToFLV(frame.FrameType)

	// Video data: 1 byte (frame type + codec) + 1 byte (AVC packet type) + 3 bytes (CTS) + payload
	var avcPacketType uint8
	if frame.FrameType == avframe.FrameTypeSequenceHeader {
		avcPacketType = AVCPacketSequenceHeader
	} else {
		avcPacketType = AVCPacketNALU
	}

	cts := int32(frame.PTS - frame.DTS)
	ctsBytes := [3]byte{
		byte(cts >> 16),
		byte(cts >> 8),
		byte(cts),
	}

	dataSize := 1 + 1 + 3 + len(frame.Payload) // video header + avc type + cts + payload
	return m.writeTag(w, TagTypeVideo, frame.DTS, dataSize, func(w io.Writer) error {
		header := []byte{(frameTypeID << 4) | codecID, avcPacketType, ctsBytes[0], ctsBytes[1], ctsBytes[2]}
		if _, err := w.Write(header); err != nil {
			return err
		}
		_, err := w.Write(frame.Payload)
		return err
	})
}

func (m *Muxer) writeAudioTag(w io.Writer, frame *avframe.AVFrame) error {
	formatID := AudioCodecToFLV(frame.Codec)
	if formatID == 0 {
		return fmt.Errorf("unsupported audio codec: %v", frame.Codec)
	}

	// Audio data: 1 byte (format + sound info) + [1 byte AAC packet type] + payload
	soundInfo := byte(0x0F) // 44100Hz, 16-bit, stereo for AAC
	firstByte := (formatID << 4) | soundInfo

	var aacPacketType byte
	hasAACType := frame.Codec == avframe.CodecAAC
	if hasAACType {
		if frame.FrameType == avframe.FrameTypeSequenceHeader {
			aacPacketType = AACPacketSequenceHeader
		} else {
			aacPacketType = AACPacketRaw
		}
	}

	headerSize := 1
	if hasAACType {
		headerSize = 2
	}
	dataSize := headerSize + len(frame.Payload)

	return m.writeTag(w, TagTypeAudio, frame.DTS, dataSize, func(w io.Writer) error {
		if hasAACType {
			if _, err := w.Write([]byte{firstByte, aacPacketType}); err != nil {
				return err
			}
		} else {
			if _, err := w.Write([]byte{firstByte}); err != nil {
				return err
			}
		}
		_, err := w.Write(frame.Payload)
		return err
	})
}

func (m *Muxer) writeTag(w io.Writer, tagType uint8, dts int64, dataSize int, writeData func(io.Writer) error) error {
	// Tag header: 11 bytes
	var header [TagHeaderSize]byte
	header[0] = tagType
	header[1] = byte(dataSize >> 16)
	header[2] = byte(dataSize >> 8)
	header[3] = byte(dataSize)

	ts := uint32(dts)
	header[4] = byte(ts >> 16)
	header[5] = byte(ts >> 8)
	header[6] = byte(ts)
	header[7] = byte(ts >> 24) // timestamp extension
	// StreamID = 0 (bytes 8-10, already zero)

	if _, err := w.Write(header[:]); err != nil {
		return err
	}

	if err := writeData(w); err != nil {
		return err
	}

	// Previous tag size
	totalSize := uint32(TagHeaderSize + dataSize)
	var prevSize [4]byte
	binary.BigEndian.PutUint32(prevSize[:], totalSize)
	_, err := w.Write(prevSize[:])
	return err
}
