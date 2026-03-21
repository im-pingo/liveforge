package fmp4

import (
	"bytes"
	"encoding/binary"

	"github.com/im-pingo/liveforge/pkg/avframe"
)

// BuildMediaSegment generates a moof+mdat segment from a slice of frames.
// audioTimescale must match the audio mdhd timescale written in the init segment
// (typically the audio sample rate, e.g. 44100). Pass 0 to fall back to raw ms values.
// Returns the concatenated moof+mdat bytes.
func BuildMediaSegment(frames []*avframe.AVFrame, sequenceNumber uint32, audioTimescale uint32) []byte {
	if len(frames) == 0 {
		return nil
	}

	// Separate video and audio frames
	var videoFrames, audioFrames []*avframe.AVFrame
	for _, f := range frames {
		if f.FrameType == avframe.FrameTypeSequenceHeader {
			continue
		}
		if f.MediaType.IsVideo() {
			videoFrames = append(videoFrames, f)
		} else if f.MediaType.IsAudio() {
			audioFrames = append(audioFrames, f)
		}
	}

	// Calculate total video data size (needed for audio data_offset)
	var videoDataSize uint32
	for _, f := range videoFrames {
		videoDataSize += uint32(len(f.Payload))
	}

	var buf bytes.Buffer

	// Build moof
	var moof bytes.Buffer

	// mfhd
	mfhd := make([]byte, 4)
	binary.BigEndian.PutUint32(mfhd, sequenceNumber)
	WriteFullBox(&moof, BoxMfhd, 0, 0, mfhd)

	// Video traf
	if len(videoFrames) > 0 {
		writeTraf(&moof, videoTrackID, videoFrames, timescaleVideo)
	}

	// Audio traf — timescale must match the audio mdhd timescale (sample rate).
	if len(audioFrames) > 0 {
		writeTraf(&moof, audioTrackID, audioFrames, audioTimescale)
	}

	moofBytes := moof.Bytes()
	moofBoxSize := 8 + len(moofBytes)

	// Build mdat: video samples first, then audio samples
	var mdatPayload bytes.Buffer
	for _, f := range videoFrames {
		mdatPayload.Write(f.Payload)
	}
	for _, f := range audioFrames {
		mdatPayload.Write(f.Payload)
	}

	// Fix data_offset in trun boxes:
	// - Video trun data_offset = moofBoxSize + mdat_header(8)
	// - Audio trun data_offset = moofBoxSize + mdat_header(8) + videoDataSize
	mdatHeaderSize := uint32(8)
	videoOffset := uint32(moofBoxSize) + mdatHeaderSize
	audioOffset := videoOffset + videoDataSize
	fixTrunDataOffsetsPerTrack(moofBytes, videoOffset, audioOffset)

	// Write moof box
	WriteBox(&buf, BoxMoof, moofBytes)

	// Write mdat box
	WriteBox(&buf, BoxMdat, mdatPayload.Bytes())

	return buf.Bytes()
}

func writeTraf(w *bytes.Buffer, trackID uint32, frames []*avframe.AVFrame, timescale uint32) {
	var traf bytes.Buffer

	// tfhd: track ID + default flags
	tfhd := make([]byte, 4)
	binary.BigEndian.PutUint32(tfhd, trackID)
	// flags: 0x020000 = default-base-is-moof
	WriteFullBox(&traf, BoxTfhd, 0, 0x020000, tfhd)

	// tfdt: base media decode time
	if len(frames) > 0 {
		var dts int64
		if timescale > 0 {
			dts = frames[0].DTS * int64(timescale) / 1000
		} else {
			dts = frames[0].DTS
		}
		tfdt := make([]byte, 8)
		binary.BigEndian.PutUint64(tfdt, uint64(dts))
		WriteFullBox(&traf, BoxTfdt, 1, 0, tfdt)
	}

	// trun: sample entries
	// flags: 0x000001 (data-offset-present) | 0x000100 (sample-duration-present) |
	//        0x000200 (sample-size-present) | 0x000400 (sample-flags-present)
	trunFlags := uint32(0x000001 | 0x000100 | 0x000200 | 0x000400)
	var trun bytes.Buffer
	binary.Write(&trun, binary.BigEndian, uint32(len(frames))) // sample_count
	binary.Write(&trun, binary.BigEndian, uint32(0))           // data_offset (placeholder)

	for i, f := range frames {
		// Duration: difference to next frame, or default 33ms for video / 23ms for audio
		var duration uint32
		if i+1 < len(frames) {
			dt := frames[i+1].DTS - f.DTS
			if timescale > 0 {
				duration = uint32(dt * int64(timescale) / 1000)
			} else {
				duration = uint32(dt)
			}
		} else {
			if f.MediaType.IsVideo() {
				duration = 3000 // ~33ms at 90kHz
			} else {
				duration = 1024 // common for AAC
			}
		}

		sampleSize := uint32(len(f.Payload))

		// Sample flags: keyframe = 0x02000000, non-keyframe = 0x01010000
		var sampleFlags uint32
		if f.FrameType.IsKeyframe() {
			sampleFlags = 0x02000000
		} else {
			sampleFlags = 0x01010000
		}

		binary.Write(&trun, binary.BigEndian, duration)
		binary.Write(&trun, binary.BigEndian, sampleSize)
		binary.Write(&trun, binary.BigEndian, sampleFlags)
	}

	WriteFullBox(&traf, BoxTrun, 0, trunFlags, trun.Bytes())
	WriteBox(w, BoxTraf, traf.Bytes())
}

// fixTrunDataOffsetsPerTrack scans moofBytes for traf boxes and patches each
// trun's data_offset. The first traf gets videoOffset, the second gets audioOffset.
func fixTrunDataOffsetsPerTrack(moofBytes []byte, videoOffset, audioOffset uint32) {
	trafType := []byte{'t', 'r', 'a', 'f'}
	trunType := []byte{'t', 'r', 'u', 'n'}

	trafIndex := 0
	offset := 0
	for offset+8 <= len(moofBytes) {
		boxSize := int(binary.BigEndian.Uint32(moofBytes[offset : offset+4]))
		if boxSize < 8 || offset+boxSize > len(moofBytes) {
			break
		}
		if bytes.Equal(moofBytes[offset+4:offset+8], trafType) {
			// Determine data_offset for this traf
			dataOffset := videoOffset
			if trafIndex > 0 {
				dataOffset = audioOffset
			}
			trafIndex++

			// Search inside traf for trun
			inner := offset + 8
			end := offset + boxSize
			for inner+8 <= end {
				innerSize := int(binary.BigEndian.Uint32(moofBytes[inner : inner+4]))
				if innerSize < 8 || inner+innerSize > end {
					break
				}
				if bytes.Equal(moofBytes[inner+4:inner+8], trunType) {
					// trun: header(8) + version+flags(4) + sample_count(4) + data_offset(4)
					dataOffsetPos := inner + 8 + 4 + 4
					if dataOffsetPos+4 <= end {
						binary.BigEndian.PutUint32(moofBytes[dataOffsetPos:], dataOffset)
					}
				}
				inner += innerSize
			}
		}
		offset += boxSize
	}
}
