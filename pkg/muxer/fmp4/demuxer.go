package fmp4

import (
	"encoding/binary"
	"fmt"

	"github.com/im-pingo/liveforge/pkg/avframe"
)

// Demuxer parses fragmented MP4 (fMP4) segments back into AVFrames.
// Initialize with an init segment (ftyp+moov) to learn track layout,
// then call Parse on each moof+mdat media segment.
type Demuxer struct {
	videoTrackID uint32
	audioTrackID uint32
	videoCodec   avframe.CodecType
	audioCodec   avframe.CodecType
	timescale    map[uint32]uint32 // trackID -> timescale
}

// NewDemuxer creates a Demuxer by parsing the init segment (ftyp+moov)
// to discover track IDs, codecs, and timescales.
func NewDemuxer(initSegment []byte) (*Demuxer, error) {
	if len(initSegment) < 8 {
		return nil, fmt.Errorf("fmp4: init segment too short (%d bytes)", len(initSegment))
	}

	d := &Demuxer{
		timescale: make(map[uint32]uint32),
	}

	// Walk top-level boxes to find moov
	offset := 0
	for offset+8 <= len(initSegment) {
		boxSize, boxType := readBoxHeader(initSegment[offset:])
		if boxSize < 8 || offset+int(boxSize) > len(initSegment) {
			break
		}
		if boxType == BoxMoov {
			if err := d.parseMoov(initSegment[offset+8 : offset+int(boxSize)]); err != nil {
				return nil, fmt.Errorf("fmp4: %w", err)
			}
		}
		offset += int(boxSize)
	}

	if d.videoTrackID == 0 && d.audioTrackID == 0 {
		return nil, fmt.Errorf("fmp4: no video or audio track found in init segment")
	}

	return d, nil
}

// Parse demuxes a moof+mdat media segment into AVFrames.
func (d *Demuxer) Parse(mediaSegment []byte) ([]*avframe.AVFrame, error) {
	if len(mediaSegment) < 8 {
		return nil, fmt.Errorf("fmp4: media segment too short (%d bytes)", len(mediaSegment))
	}

	var moofData []byte
	var moofOffset int
	var mdatData []byte

	// Walk top-level boxes to find moof and mdat
	offset := 0
	for offset+8 <= len(mediaSegment) {
		boxSize, boxType := readBoxHeader(mediaSegment[offset:])
		if boxSize < 8 || offset+int(boxSize) > len(mediaSegment) {
			break
		}
		if boxType == BoxMoof {
			moofOffset = offset
			moofData = mediaSegment[offset+8 : offset+int(boxSize)]
		} else if boxType == BoxMdat {
			mdatData = mediaSegment[offset+8 : offset+int(boxSize)]
		}
		offset += int(boxSize)
	}

	if moofData == nil {
		return nil, fmt.Errorf("fmp4: moof box not found in media segment")
	}
	if mdatData == nil {
		return nil, fmt.Errorf("fmp4: mdat box not found in media segment")
	}

	// Parse all traf boxes inside moof
	var allFrames []*avframe.AVFrame
	inner := 0
	for inner+8 <= len(moofData) {
		boxSize, boxType := readBoxHeader(moofData[inner:])
		if boxSize < 8 || inner+int(boxSize) > len(moofData) {
			break
		}
		if boxType == BoxTraf {
			trafData := moofData[inner+8 : inner+int(boxSize)]
			frames, err := d.parseTraf(trafData, mediaSegment, moofOffset)
			if err != nil {
				return nil, fmt.Errorf("fmp4: %w", err)
			}
			allFrames = append(allFrames, frames...)
		}
		inner += int(boxSize)
	}

	_ = mdatData // mdat is accessed via absolute offsets from mediaSegment

	return allFrames, nil
}

// parseMoov walks moov children to find trak boxes.
func (d *Demuxer) parseMoov(data []byte) error {
	offset := 0
	for offset+8 <= len(data) {
		boxSize, boxType := readBoxHeader(data[offset:])
		if boxSize < 8 || offset+int(boxSize) > len(data) {
			break
		}
		if boxType == BoxTrak {
			if err := d.parseTrak(data[offset+8 : offset+int(boxSize)]); err != nil {
				return err
			}
		}
		offset += int(boxSize)
	}
	return nil
}

// parseTrak extracts track ID from tkhd, handler type from hdlr, and timescale from mdhd.
func (d *Demuxer) parseTrak(data []byte) error {
	var trackID uint32
	var handlerType [4]byte
	var timescale uint32

	offset := 0
	for offset+8 <= len(data) {
		boxSize, boxType := readBoxHeader(data[offset:])
		if boxSize < 8 || offset+int(boxSize) > len(data) {
			break
		}
		boxPayload := data[offset+8 : offset+int(boxSize)]

		if boxType == BoxTkhd {
			trackID = parseTkhd(boxPayload)
		} else if boxType == BoxMdia {
			ht, ts := parseMdia(boxPayload)
			handlerType = ht
			timescale = ts
		}
		offset += int(boxSize)
	}

	if trackID == 0 {
		return nil // skip tracks without valid tkhd
	}

	d.timescale[trackID] = timescale

	switch string(handlerType[:]) {
	case "vide":
		d.videoTrackID = trackID
		d.videoCodec = avframe.CodecH264 // default; could be refined from stsd
	case "soun":
		d.audioTrackID = trackID
		d.audioCodec = avframe.CodecAAC // default; could be refined from stsd
	}

	return nil
}

// parseTkhd extracts track_ID from a tkhd full box payload.
// Full box payload starts with version(1)+flags(3), then for v0:
// creation_time(4) + modification_time(4) + track_ID(4)
func parseTkhd(data []byte) uint32 {
	if len(data) < 4+12 {
		return 0
	}
	// version+flags = 4 bytes, creation_time(4), modification_time(4), track_ID(4)
	version := data[0]
	if version == 0 {
		if len(data) < 4+12 {
			return 0
		}
		return binary.BigEndian.Uint32(data[12:16])
	}
	// version 1: creation_time(8) + modification_time(8) + track_ID(4)
	if len(data) < 4+20 {
		return 0
	}
	return binary.BigEndian.Uint32(data[20:24])
}

// parseMdia walks mdia children to find hdlr (handler_type) and mdhd (timescale).
func parseMdia(data []byte) ([4]byte, uint32) {
	var handlerType [4]byte
	var timescale uint32

	offset := 0
	for offset+8 <= len(data) {
		boxSize, boxType := readBoxHeader(data[offset:])
		if boxSize < 8 || offset+int(boxSize) > len(data) {
			break
		}
		boxPayload := data[offset+8 : offset+int(boxSize)]

		if boxType == BoxHdlr {
			handlerType = parseHdlr(boxPayload)
		} else if boxType == BoxMdhd {
			timescale = parseMdhd(boxPayload)
		}
		offset += int(boxSize)
	}

	return handlerType, timescale
}

// parseHdlr extracts handler_type from hdlr full box payload.
// Layout after version+flags(4): pre_defined(4) + handler_type(4)
func parseHdlr(data []byte) [4]byte {
	var ht [4]byte
	if len(data) < 4+8 {
		return ht
	}
	copy(ht[:], data[8:12])
	return ht
}

// parseMdhd extracts timescale from mdhd full box payload.
// v0: version+flags(4) + creation_time(4) + modification_time(4) + timescale(4)
// v1: version+flags(4) + creation_time(8) + modification_time(8) + timescale(4)
func parseMdhd(data []byte) uint32 {
	if len(data) < 4+12 {
		return 0
	}
	version := data[0]
	if version == 0 {
		return binary.BigEndian.Uint32(data[12:16])
	}
	// version 1
	if len(data) < 4+20 {
		return 0
	}
	return binary.BigEndian.Uint32(data[20:24])
}

// trafInfo holds parsed information from a traf box.
type trafInfo struct {
	trackID               uint32
	baseDecodeTime        uint64
	defaultSampleDuration uint32
	defaultSampleSize     uint32
	defaultSampleFlags    uint32
	samples               []sampleEntry
	dataOffset            int32
}

// sampleEntry holds per-sample metadata from a trun box.
type sampleEntry struct {
	duration            uint32
	size                uint32
	flags               uint32
	compositionTimeOffset int32
}

// parseTraf parses a traf box to extract tfhd, tfdt, and trun data,
// then extracts sample payloads from the full media segment.
func (d *Demuxer) parseTraf(trafData []byte, mediaSegment []byte, moofOffset int) ([]*avframe.AVFrame, error) {
	info := &trafInfo{}

	offset := 0
	for offset+8 <= len(trafData) {
		boxSize, boxType := readBoxHeader(trafData[offset:])
		if boxSize < 8 || offset+int(boxSize) > len(trafData) {
			break
		}
		boxPayload := trafData[offset+8 : offset+int(boxSize)]

		switch boxType {
		case BoxTfhd:
			parseTfhd(boxPayload, info)
		case BoxTfdt:
			parseTfdt(boxPayload, info)
		case BoxTrun:
			parseTrun(boxPayload, info)
		}
		offset += int(boxSize)
	}

	// Determine media type and codec from track ID
	var mediaType avframe.MediaType
	var codec avframe.CodecType
	if info.trackID == d.videoTrackID {
		mediaType = avframe.MediaTypeVideo
		codec = d.videoCodec
	} else if info.trackID == d.audioTrackID {
		mediaType = avframe.MediaTypeAudio
		codec = d.audioCodec
	} else {
		return nil, nil // unknown track, skip
	}

	ts := d.timescale[info.trackID]
	if ts == 0 {
		ts = 1000 // fallback to ms
	}

	// Calculate the absolute byte offset where samples start.
	// data_offset in trun is relative to the start of the containing moof box.
	sampleDataStart := moofOffset + int(info.dataOffset)

	var frames []*avframe.AVFrame
	currentOffset := sampleDataStart
	currentDecodeTime := info.baseDecodeTime

	for _, s := range info.samples {
		size := s.size
		if size == 0 {
			size = info.defaultSampleSize
		}
		if currentOffset+int(size) > len(mediaSegment) {
			return nil, fmt.Errorf("sample data exceeds segment bounds at offset %d (size %d, segment len %d)",
				currentOffset, size, len(mediaSegment))
		}

		// Copy payload to avoid aliasing the input slice
		payload := make([]byte, size)
		copy(payload, mediaSegment[currentOffset:currentOffset+int(size)])

		// Convert decode time from track timescale to milliseconds
		dtsMs := int64(currentDecodeTime) * 1000 / int64(ts)

		// PTS = DTS + composition_time_offset (converted to ms)
		ptsMs := dtsMs
		if s.compositionTimeOffset != 0 {
			ptsMs = dtsMs + int64(s.compositionTimeOffset)*1000/int64(ts)
		}

		// Determine frame type from sample flags
		sampleFlags := s.flags
		if sampleFlags == 0 {
			sampleFlags = info.defaultSampleFlags
		}
		frameType := avframe.FrameTypeInterframe
		if isKeyframeSampleFlags(sampleFlags) {
			frameType = avframe.FrameTypeKeyframe
		}

		frames = append(frames, &avframe.AVFrame{
			MediaType: mediaType,
			Codec:     codec,
			FrameType: frameType,
			DTS:       dtsMs,
			PTS:       ptsMs,
			Payload:   payload,
		})

		duration := s.duration
		if duration == 0 {
			duration = info.defaultSampleDuration
		}
		currentDecodeTime += uint64(duration)
		currentOffset += int(size)
	}

	return frames, nil
}

// parseTfhd parses a tfhd full box payload.
// Full box: version(1) + flags(3) + track_ID(4) + optional fields based on flags.
func parseTfhd(data []byte, info *trafInfo) {
	if len(data) < 8 {
		return
	}
	flags := uint32(data[1])<<16 | uint32(data[2])<<8 | uint32(data[3])
	info.trackID = binary.BigEndian.Uint32(data[4:8])

	offset := 8
	// 0x000001: base-data-offset-present
	if flags&0x000001 != 0 {
		offset += 8
	}
	// 0x000002: sample-description-index-present
	if flags&0x000002 != 0 {
		offset += 4
	}
	// 0x000008: default-sample-duration-present
	if flags&0x000008 != 0 {
		if offset+4 <= len(data) {
			info.defaultSampleDuration = binary.BigEndian.Uint32(data[offset : offset+4])
		}
		offset += 4
	}
	// 0x000010: default-sample-size-present
	if flags&0x000010 != 0 {
		if offset+4 <= len(data) {
			info.defaultSampleSize = binary.BigEndian.Uint32(data[offset : offset+4])
		}
		offset += 4
	}
	// 0x000020: default-sample-flags-present
	if flags&0x000020 != 0 {
		if offset+4 <= len(data) {
			info.defaultSampleFlags = binary.BigEndian.Uint32(data[offset : offset+4])
		}
	}
}

// parseTfdt parses a tfdt full box payload.
// v0: version(1) + flags(3) + baseMediaDecodeTime(4)
// v1: version(1) + flags(3) + baseMediaDecodeTime(8)
func parseTfdt(data []byte, info *trafInfo) {
	if len(data) < 8 {
		return
	}
	version := data[0]
	if version == 0 {
		info.baseDecodeTime = uint64(binary.BigEndian.Uint32(data[4:8]))
	} else {
		if len(data) < 12 {
			return
		}
		info.baseDecodeTime = binary.BigEndian.Uint64(data[4:12])
	}
}

// parseTrun parses a trun full box payload.
// Layout: version(1) + flags(3) + sample_count(4) + [data_offset(4)] + [first_sample_flags(4)]
// Then per sample: [duration(4)] [size(4)] [flags(4)] [composition_time_offset(4)]
func parseTrun(data []byte, info *trafInfo) {
	if len(data) < 8 {
		return
	}
	flags := uint32(data[1])<<16 | uint32(data[2])<<8 | uint32(data[3])
	sampleCount := binary.BigEndian.Uint32(data[4:8])

	offset := 8

	// 0x000001: data-offset-present
	if flags&0x000001 != 0 {
		if offset+4 > len(data) {
			return
		}
		info.dataOffset = int32(binary.BigEndian.Uint32(data[offset : offset+4]))
		offset += 4
	}

	// 0x000004: first-sample-flags-present
	var firstSampleFlags uint32
	hasFirstSampleFlags := false
	if flags&0x000004 != 0 {
		if offset+4 > len(data) {
			return
		}
		firstSampleFlags = binary.BigEndian.Uint32(data[offset : offset+4])
		hasFirstSampleFlags = true
		offset += 4
	}

	hasDuration := flags&0x000100 != 0
	hasSize := flags&0x000200 != 0
	hasFlags := flags&0x000400 != 0
	hasCTO := flags&0x000800 != 0

	for i := uint32(0); i < sampleCount; i++ {
		var entry sampleEntry

		if hasDuration {
			if offset+4 > len(data) {
				return
			}
			entry.duration = binary.BigEndian.Uint32(data[offset : offset+4])
			offset += 4
		}
		if hasSize {
			if offset+4 > len(data) {
				return
			}
			entry.size = binary.BigEndian.Uint32(data[offset : offset+4])
			offset += 4
		}
		if hasFlags {
			if offset+4 > len(data) {
				return
			}
			entry.flags = binary.BigEndian.Uint32(data[offset : offset+4])
			offset += 4
		}
		if hasCTO {
			if offset+4 > len(data) {
				return
			}
			entry.compositionTimeOffset = int32(binary.BigEndian.Uint32(data[offset : offset+4]))
			offset += 4
		}

		// Apply first_sample_flags to the first sample if per-sample flags are absent
		if i == 0 && hasFirstSampleFlags && !hasFlags {
			entry.flags = firstSampleFlags
		}

		info.samples = append(info.samples, entry)
	}
}

// isKeyframeSampleFlags checks sample flags to determine if a sample is a keyframe.
// The muxer writes 0x02000000 for keyframes and 0x01010000 for non-keyframes.
// In ISO 14496-12, bits 25-24 (0-indexed) encode sample_depends_on:
//   0 = unknown, 1 = depends on others (non-key), 2 = does NOT depend (key)
// Also bit 16 (sample_is_non_sync_sample): 0 = sync/key, 1 = non-sync.
func isKeyframeSampleFlags(flags uint32) bool {
	dependsOn := (flags >> 24) & 0x03
	if dependsOn == 2 {
		return true // explicitly does not depend on other samples
	}
	isNonSync := (flags >> 16) & 0x01
	if dependsOn == 0 && isNonSync == 0 {
		return true // unknown dependency + sync sample = treat as keyframe
	}
	return false
}

// readBoxHeader reads the size (4 bytes) and type (4 bytes) from a box header.
func readBoxHeader(data []byte) (uint32, [4]byte) {
	var boxType [4]byte
	if len(data) < 8 {
		return 0, boxType
	}
	size := binary.BigEndian.Uint32(data[0:4])
	copy(boxType[:], data[4:8])
	return size, boxType
}
