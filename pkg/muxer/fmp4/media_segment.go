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
	return buildMediaSegment(frames, sequenceNumber, audioTimescale, 0)
}

func buildMediaSegment(frames []*avframe.AVFrame, sequenceNumber uint32, audioTimescale uint32, videoEndDTS int64) []byte {
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
			// Strip non-VCL NALUs (SPS/PPS/SEI/AUD) from H.264 AVCC payloads.
			// These parameter sets are already in the init segment's avcC box;
			// duplicating them in samples can cause browser MSE to reject the
			// segment with bufferAppendError.
			if f.Codec == avframe.CodecH264 {
				filtered := filterH264VCLNALUs(f.Payload)
				if len(filtered) == 0 {
					continue
				}
				if len(filtered) != len(f.Payload) {
					// Create a shallow copy with the filtered payload
					cf := *f
					cf.Payload = filtered
					videoFrames = append(videoFrames, &cf)
					continue
				}
			}
			if f.Codec == avframe.CodecH265 {
				filtered := filterH265ParameterSetNALUs(f.Payload)
				if len(filtered) == 0 {
					continue
				}
				if len(filtered) != len(f.Payload) {
					cf := *f
					cf.Payload = filtered
					videoFrames = append(videoFrames, &cf)
					continue
				}
			}
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
		writeTraf(&moof, videoTrackID, videoFrames, timescaleVideo, videoEndDTS)
	}

	// Audio traf — timescale must match the audio mdhd timescale (sample rate).
	if len(audioFrames) > 0 {
		writeTraf(&moof, audioTrackID, audioFrames, audioTimescale, 0)
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

func writeTraf(w *bytes.Buffer, trackID uint32, frames []*avframe.AVFrame, timescale uint32, endDTS int64) {
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
	//        0x000200 (sample-size-present) | 0x000400 (sample-flags-present) |
	//        0x000800 (sample-composition-time-offsets-present)
	trunFlags := uint32(0x000001 | 0x000100 | 0x000200 | 0x000400 | 0x000800)
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
			if f.MediaType.IsVideo() && endDTS > f.DTS {
				duration = uint32((endDTS - f.DTS) * int64(timescale) / 1000)
			} else if f.MediaType.IsVideo() {
				duration = 3000 // ~33ms at 90kHz
			} else {
				duration = finalAudioSampleDuration(f, timescale)
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

		// Composition time offset (PTS - DTS) scaled to track timescale.
		// Required for B-frame content where PTS != DTS.
		cts := f.PTS - f.DTS
		var sampleCTO int32
		if timescale > 0 {
			sampleCTO = int32(cts * int64(timescale) / 1000)
		} else {
			sampleCTO = int32(cts)
		}

		binary.Write(&trun, binary.BigEndian, duration)
		binary.Write(&trun, binary.BigEndian, sampleSize)
		binary.Write(&trun, binary.BigEndian, sampleFlags)
		binary.Write(&trun, binary.BigEndian, sampleCTO)
	}

	// Version 1 trun uses signed composition time offsets (required for B-frames).
	WriteFullBox(&traf, BoxTrun, 1, trunFlags, trun.Bytes())
	WriteBox(w, BoxTraf, traf.Bytes())
}

func finalAudioSampleDuration(frame *avframe.AVFrame, timescale uint32) uint32 {
	if frame.Codec != avframe.CodecOpus {
		return 1024 // AAC-LC and HE-AAC use 1024 samples per access unit.
	}

	samples, ok := opusPacketDurationSamples(frame.Payload)
	if !ok {
		samples = 960 // 20 ms at the mandatory Opus 48 kHz RTP clock.
	}
	if timescale == 0 {
		durationMs := samples * 1000 / 48000
		if durationMs == 0 {
			return 1
		}
		return durationMs
	}
	return uint32(uint64(samples) * uint64(timescale) / 48000)
}

func opusPacketDurationSamples(payload []byte) (uint32, bool) {
	if len(payload) == 0 {
		return 0, false
	}

	config := payload[0] >> 3
	var samplesPerFrame uint32
	switch {
	case config < 12:
		samplesPerFrame = [...]uint32{480, 960, 1920, 2880}[config&0x03]
	case config < 16:
		samplesPerFrame = 480 << (config & 0x01)
	default:
		samplesPerFrame = 120 << (config & 0x03)
	}

	var frameCount uint32
	switch payload[0] & 0x03 {
	case 0:
		frameCount = 1
	case 1, 2:
		frameCount = 2
	case 3:
		if len(payload) < 2 {
			return 0, false
		}
		frameCount = uint32(payload[1] & 0x3F)
		if frameCount == 0 {
			return 0, false
		}
	}

	duration := samplesPerFrame * frameCount
	if duration == 0 || duration > 5760 {
		return 0, false
	}
	return duration, true
}

// filterH264VCLNALUs strips non-VCL NAL units from an AVCC (4-byte length-prefixed)
// H.264 payload, keeping only slice NALUs (types 1-5). SPS(7), PPS(8), SEI(6),
// and AUD(9) are removed since they are already in the init segment's avcC box
// and can cause browser MSE to reject fMP4 segments.
func filterH264VCLNALUs(data []byte) []byte {
	if len(data) < 5 {
		return data
	}

	needsFilter := false
	offset := 0
	for offset+4 < len(data) {
		nalLen := int(binary.BigEndian.Uint32(data[offset:]))
		if nalLen <= 0 || offset+4+nalLen > len(data) {
			break
		}
		nalType := data[offset+4] & 0x1F
		if nalType == 6 || nalType == 7 || nalType == 8 || nalType == 9 {
			needsFilter = true
			break
		}
		offset += 4 + nalLen
	}
	if !needsFilter {
		return data
	}

	var result []byte
	offset = 0
	for offset+4 < len(data) {
		nalLen := int(binary.BigEndian.Uint32(data[offset:]))
		if nalLen <= 0 || offset+4+nalLen > len(data) {
			break
		}
		nalType := data[offset+4] & 0x1F
		// Keep VCL NALUs (1=non-IDR slice, 2=slice A, 3=slice B, 4=slice C, 5=IDR)
		if nalType >= 1 && nalType <= 5 {
			result = append(result, data[offset:offset+4+nalLen]...)
		}
		offset += 4 + nalLen
	}
	return result
}

// filterH265ParameterSetNALUs removes VPS/SPS/PPS from length-prefixed samples.
// hvc1 carries these parameter sets exclusively in the hvcC configuration box.
func filterH265ParameterSetNALUs(data []byte) []byte {
	if len(data) < 6 {
		return data
	}

	needsFilter := false
	for offset := 0; offset+6 <= len(data); {
		nalLen := int(binary.BigEndian.Uint32(data[offset:]))
		if nalLen < 2 || offset+4+nalLen > len(data) {
			return data
		}
		nalType := (data[offset+4] >> 1) & 0x3F
		if nalType >= 32 && nalType <= 34 {
			needsFilter = true
		}
		offset += 4 + nalLen
		if offset == len(data) {
			break
		}
	}
	if !needsFilter {
		return data
	}

	result := make([]byte, 0, len(data))
	for offset := 0; offset+6 <= len(data); {
		nalLen := int(binary.BigEndian.Uint32(data[offset:]))
		if nalLen < 2 || offset+4+nalLen > len(data) {
			return data
		}
		nalType := (data[offset+4] >> 1) & 0x3F
		if nalType < 32 || nalType > 34 {
			result = append(result, data[offset:offset+4+nalLen]...)
		}
		offset += 4 + nalLen
	}
	return result
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
