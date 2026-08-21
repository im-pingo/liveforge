package flv

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/im-pingo/liveforge/pkg/avframe"
)

// Muxer packs AVFrames into FLV tags.
type Muxer struct {
	videoMode EncodingMode
	audioMode EncodingMode
}

// NewMuxer creates a new FLV muxer.
func NewMuxer() *Muxer {
	return NewMuxerWithModes(EncodingAuto, EncodingAuto)
}

// NewMuxerWithModes creates a muxer with independent video and audio modes.
func NewMuxerWithModes(videoMode, audioMode EncodingMode) *Muxer {
	return &Muxer{videoMode: videoMode, audioMode: audioMode}
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
		return m.writeVideoTag(w, frame, m.videoMode)
	}
	if frame.MediaType.IsAudio() {
		return m.writeAudioTag(w, frame, m.audioMode)
	}
	return fmt.Errorf("unsupported media type: %v", frame.MediaType)
}

func (m *Muxer) writeVideoTag(w io.Writer, frame *avframe.AVFrame, mode EncodingMode) error {
	if mode == EncodingAuto {
		if frame.Codec == avframe.CodecH264 {
			mode = EncodingClassic
		} else {
			mode = EncodingEnhanced
		}
	}
	if mode == EncodingClassic {
		return m.writeClassicVideoTag(w, frame)
	}
	if mode == EncodingEnhanced {
		return m.writeEnhancedVideoTag(w, frame)
	}
	return fmt.Errorf("unsupported video encoding mode: %d", mode)
}

func (m *Muxer) writeClassicVideoTag(w io.Writer, frame *avframe.AVFrame) error {
	if frame.Codec != avframe.CodecH264 {
		return fmt.Errorf("unsupported classic video codec: %v", frame.Codec)
	}
	codecID := VideoCodecToFLV(frame.Codec)
	if codecID == 0 {
		return fmt.Errorf("unsupported video codec: %v", frame.Codec)
	}

	frameTypeID := AVFrameTypeToFLV(frame.FrameType)

	var avcPacketType uint8
	if frame.FrameType == avframe.FrameTypeSequenceHeader {
		avcPacketType = AVCPacketSequenceHeader
	} else {
		avcPacketType = AVCPacketNALU
	}

	ctsBytes, err := encodeSI24(frame.PTS - frame.DTS)
	if err != nil {
		return err
	}

	dataSize := 1 + 1 + 3 + len(frame.Payload)
	return m.writeTag(w, TagTypeVideo, frame.DTS, dataSize, func(w io.Writer) error {
		header := []byte{(frameTypeID << 4) | codecID, avcPacketType, ctsBytes[0], ctsBytes[1], ctsBytes[2]}
		if _, err := w.Write(header); err != nil {
			return err
		}
		_, err := w.Write(frame.Payload)
		return err
	})
}

func (m *Muxer) writeEnhancedVideoTag(w io.Writer, frame *avframe.AVFrame) error {
	fourcc := VideoCodecToFourCC(frame.Codec)
	if fourcc == [4]byte{} {
		return fmt.Errorf("unsupported enhanced video codec: %v", frame.Codec)
	}

	// ExVideoTagHeader: 1 byte (0x80 | frameType<<4 | packetType) + 4 bytes FourCC
	var packetType uint8
	if frame.FrameType == avframe.FrameTypeSequenceHeader {
		packetType = ExVideoPacketSequenceStart
	} else {
		packetType = ExVideoPacketCodedFrames
	}

	frameTypeNibble := AVFrameTypeToFLV(frame.FrameType)
	firstByte := byte(0x80) | (frameTypeNibble << 4) | packetType

	headerBytes := []byte{
		firstByte,
		fourcc[0], fourcc[1], fourcc[2], fourcc[3],
	}
	if packetType == ExVideoPacketCodedFrames {
		if frame.Codec == avframe.CodecH264 || frame.Codec == avframe.CodecH265 {
			ctsBytes, err := encodeSI24(frame.PTS - frame.DTS)
			if err != nil {
				return err
			}
			headerBytes = append(headerBytes, ctsBytes[:]...)
		} else if frame.PTS != frame.DTS {
			return fmt.Errorf("enhanced codec %v cannot encode composition offset", frame.Codec)
		}
	}

	dataSize := len(headerBytes) + len(frame.Payload)
	return m.writeTag(w, TagTypeVideo, frame.DTS, dataSize, func(w io.Writer) error {
		if _, err := w.Write(headerBytes); err != nil {
			return err
		}
		_, err := w.Write(frame.Payload)
		return err
	})
}

func (m *Muxer) writeAudioTag(w io.Writer, frame *avframe.AVFrame, mode EncodingMode) error {
	if mode == EncodingAuto {
		if frame.Codec == avframe.CodecAAC || frame.Codec == avframe.CodecMP3 {
			mode = EncodingClassic
		} else {
			mode = EncodingEnhanced
		}
	}
	if mode == EncodingEnhanced {
		return m.writeEnhancedAudioTag(w, frame)
	}
	if mode != EncodingClassic {
		return fmt.Errorf("unsupported audio encoding mode: %d", mode)
	}
	if frame.Codec != avframe.CodecAAC && frame.Codec != avframe.CodecMP3 {
		return fmt.Errorf("unsupported classic audio codec: %v", frame.Codec)
	}

	formatID := AudioCodecToFLV(frame.Codec)

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

func (m *Muxer) writeEnhancedAudioTag(w io.Writer, frame *avframe.AVFrame) error {
	fourcc := AudioCodecToFourCC(frame.Codec)
	if fourcc == "" {
		return fmt.Errorf("unsupported enhanced audio codec: %v", frame.Codec)
	}

	var packetType byte
	if frame.FrameType == avframe.FrameTypeSequenceHeader {
		packetType = 0 // sequence start
	} else {
		packetType = 1 // coded frames
	}

	// Enhanced audio header: ExHeader + packet type + FourCC + payload.
	headerBytes := make([]byte, 1, 1+len(fourcc))
	headerBytes[0] = (AudioFormatExHeader << 4) | packetType
	headerBytes = append(headerBytes, fourcc...)
	dataSize := len(headerBytes) + len(frame.Payload)

	return m.writeTag(w, TagTypeAudio, frame.DTS, dataSize, func(w io.Writer) error {
		if _, err := w.Write(headerBytes); err != nil {
			return err
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
