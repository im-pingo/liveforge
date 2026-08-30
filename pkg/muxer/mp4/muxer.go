package mp4

import (
	"bytes"
	"encoding/binary"
	"io"
	"math"

	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/codec/aac"
	"github.com/im-pingo/liveforge/pkg/codec/h264"
)

// Muxer produces a classic MP4 file (ftyp + mdat + moov).
// Frames are written sequentially; moov is built and written on Finalize().
type Muxer struct {
	videoCodec avframe.CodecType
	audioCodec avframe.CodecType
	timescale  uint32

	videoSeqHeader []byte // raw AVC/HEVC decoder config
	audioSeqHeader []byte // raw AudioSpecificConfig
	sps, pps       []byte // extracted from AVC record

	videoSamples []sampleEntry
	audioSamples []sampleEntry

	mdatOffset int64 // byte offset where mdat data starts (after 8-byte header)
	mdatSize   int64 // total bytes in mdat (excluding box header)

	audioSampleRate uint32
	audioChannels   uint16
	prevVideoDTS    int64
	prevAudioDTS    int64
}

type sampleEntry struct {
	size     uint32
	duration uint32
	offset   int64
	isSync   bool
	cts      int32 // composition time offset (PTS - DTS)
}

// NewMuxer creates an MP4 muxer.
func NewMuxer(videoCodec, audioCodec avframe.CodecType) *Muxer {
	return &Muxer{
		videoCodec:      videoCodec,
		audioCodec:      audioCodec,
		timescale:       90000,
		audioSampleRate: 44100,
		audioChannels:   2,
		prevVideoDTS:    -1,
		prevAudioDTS:    -1,
	}
}

// SetAudioParams sets audio sample rate and channels from stream metadata.
func (m *Muxer) SetAudioParams(sampleRate uint32, channels uint16) {
	if sampleRate > 0 {
		m.audioSampleRate = sampleRate
	}
	if channels > 0 {
		m.audioChannels = channels
	}
}

// WriteFtyp writes the ftyp box.
func (m *Muxer) WriteFtyp(w io.Writer) error {
	var buf bytes.Buffer
	buf.Write([]byte("isom"))     // major brand
	putU32Buf(&buf, 0x00000200)   // minor version
	buf.Write([]byte("isomiso2")) // compatible brands
	if m.videoCodec == avframe.CodecH264 {
		buf.Write([]byte("avc1"))
	}
	buf.Write([]byte("mp41"))

	writeBox(w, [4]byte{'f', 't', 'y', 'p'}, buf.Bytes())
	return nil
}

// WriteMdatHeader writes the mdat box header (size placeholder, filled by Finalize).
// Returns the offset of the mdat size field for later fixup.
func (m *Muxer) WriteMdatHeader(w io.WriteSeeker) (int64, error) {
	offset, _ := w.Seek(0, io.SeekCurrent)
	m.mdatOffset = offset + 8 // data starts after the 8-byte box header

	var hdr [8]byte
	putU32(hdr[0:4], 0) // placeholder — will be fixed up
	copy(hdr[4:8], []byte("mdat"))
	_, err := w.Write(hdr[:])
	return offset, err
}

// WriteFrame appends a frame to the mdat region and records sample metadata.
// Returns the number of bytes written.
func (m *Muxer) WriteFrame(w io.WriteSeeker, frame *avframe.AVFrame) (int, error) {
	if frame.FrameType == avframe.FrameTypeSequenceHeader {
		if frame.MediaType.IsVideo() {
			m.videoCodec = frame.Codec
			m.videoSeqHeader = append([]byte(nil), frame.Payload...)
			if m.videoCodec == avframe.CodecH264 && len(m.videoSeqHeader) > 5 {
				sps, pps, err := h264.ExtractSPSPPSFromAVCRecord(m.videoSeqHeader)
				if err == nil {
					m.sps = sps
					m.pps = pps
				}
			}
		} else if frame.MediaType.IsAudio() {
			m.audioCodec = frame.Codec
			m.audioSeqHeader = append([]byte(nil), frame.Payload...)
			if m.audioCodec == avframe.CodecAAC && len(m.audioSeqHeader) >= 2 {
				info, err := aac.ParseAudioSpecificConfig(m.audioSeqHeader)
				if err == nil {
					m.audioSampleRate = uint32(info.SampleRate)
					m.audioChannels = uint16(info.Channels)
				}
			}
		}
		return 0, nil
	}

	offset, _ := w.Seek(0, io.SeekCurrent)
	data := frame.Payload
	n, err := w.Write(data)
	if err != nil {
		return 0, err
	}

	m.mdatSize += int64(n)

	duration := uint32(0)
	previousDTS := &m.prevVideoDTS
	timescale := m.timescale
	if frame.MediaType.IsAudio() {
		previousDTS = &m.prevAudioDTS
		timescale = m.audioSampleRate
	}
	if *previousDTS >= 0 {
		d := frame.DTS - *previousDTS
		duration = scaleDurationMillis(d, timescale)
	}

	entry := sampleEntry{
		size:   uint32(n),
		offset: offset,
		isSync: frame.FrameType.IsKeyframe() || frame.FrameType == avframe.FrameTypeSequenceHeader,
		cts:    scaleCompositionOffsetMillis(frame.PTS-frame.DTS, timescale),
	}

	if frame.MediaType.IsVideo() {
		if len(m.videoSamples) > 0 {
			m.videoSamples[len(m.videoSamples)-1].duration = duration
		}
		entry.isSync = frame.FrameType.IsKeyframe()
		m.videoSamples = append(m.videoSamples, entry)
		m.prevVideoDTS = frame.DTS
	} else if frame.MediaType.IsAudio() {
		if len(m.audioSamples) > 0 {
			m.audioSamples[len(m.audioSamples)-1].duration = duration
		}
		m.audioSamples = append(m.audioSamples, entry)
		m.prevAudioDTS = frame.DTS
	}

	return n, nil
}

// Finalize writes the moov box and fixes up the mdat size.
func (m *Muxer) Finalize(w io.WriteSeeker) error {
	// Fix up mdat box size
	end, _ := w.Seek(0, io.SeekCurrent)
	mdatTotalSize := end - (m.mdatOffset - 8)
	w.Seek(m.mdatOffset-8, io.SeekStart)
	var sizeBytes [4]byte
	binary.BigEndian.PutUint32(sizeBytes[:], uint32(mdatTotalSize))
	w.Write(sizeBytes[:])
	w.Seek(end, io.SeekStart)

	// Set last sample durations if missing
	if len(m.videoSamples) > 0 && m.videoSamples[len(m.videoSamples)-1].duration == 0 {
		if len(m.videoSamples) > 1 {
			m.videoSamples[len(m.videoSamples)-1].duration = m.videoSamples[len(m.videoSamples)-2].duration
		} else {
			m.videoSamples[0].duration = 3000 // ~33ms at 90kHz
		}
	}
	if len(m.audioSamples) > 0 && m.audioSamples[len(m.audioSamples)-1].duration == 0 {
		if len(m.audioSamples) > 1 {
			m.audioSamples[len(m.audioSamples)-1].duration = m.audioSamples[len(m.audioSamples)-2].duration
		} else {
			m.audioSamples[0].duration = 1024 // typical AAC frame
		}
	}

	moov := m.buildMoov()
	_, err := w.Write(moov)
	return err
}

func (m *Muxer) buildMoov() []byte {
	var buf bytes.Buffer

	mvhd := m.buildMvhd()
	writeBox(&buf, [4]byte{'m', 'v', 'h', 'd'}, mvhd)

	if len(m.videoSamples) > 0 {
		trak := m.buildTrak(true)
		writeBox(&buf, [4]byte{'t', 'r', 'a', 'k'}, trak)
	}

	if len(m.audioSamples) > 0 {
		trak := m.buildTrak(false)
		writeBox(&buf, [4]byte{'t', 'r', 'a', 'k'}, trak)
	}

	var out bytes.Buffer
	writeBox(&out, [4]byte{'m', 'o', 'o', 'v'}, buf.Bytes())
	return out.Bytes()
}

func (m *Muxer) totalDuration(samples []sampleEntry) uint64 {
	var total uint64
	for _, s := range samples {
		total += uint64(s.duration)
	}
	return total
}

func (m *Muxer) buildMvhd() []byte {
	d := scaleDurationUnits(m.totalDuration(m.videoSamples), m.timescale, m.timescale)
	if len(m.audioSamples) > 0 {
		ad := scaleDurationUnits(m.totalDuration(m.audioSamples), m.audioSampleRate, m.timescale)
		if ad > d {
			d = ad
		}
	}

	buf := make([]byte, 100)
	putU32(buf[0:4], 0)             // version + flags
	putU32(buf[4:8], 0)             // creation time
	putU32(buf[8:12], 0)            // modification time
	putU32(buf[12:16], m.timescale) // timescale
	putU32(buf[16:20], d)           // duration
	putU32(buf[20:24], 0x00010000)  // rate 1.0
	putU16(buf[24:26], 0x0100)      // volume 1.0

	// reserved + matrix + predefined
	copy(buf[26:], make([]byte, 10+36+24))

	// next_track_id
	nextTrack := uint32(2)
	if len(m.audioSamples) > 0 {
		nextTrack = 3
	}
	putU32(buf[96:100], nextTrack)

	return buf
}

func (m *Muxer) buildTrak(isVideo bool) []byte {
	var buf bytes.Buffer

	trackID := uint32(1)
	if !isVideo {
		trackID = 2
	}

	var samples []sampleEntry
	ts := m.timescale
	if isVideo {
		samples = m.videoSamples
	} else {
		samples = m.audioSamples
		ts = m.audioSampleRate
	}

	dur := m.totalDuration(samples)

	tkhd := m.buildTkhd(trackID, scaleDurationUnits(dur, ts, m.timescale), isVideo)
	writeFullBox(&buf, [4]byte{'t', 'k', 'h', 'd'}, 0, 3, tkhd)

	mdia := m.buildMdia(isVideo, ts, samples)
	writeBox(&buf, [4]byte{'m', 'd', 'i', 'a'}, mdia)

	return buf.Bytes()
}

func (m *Muxer) buildTkhd(trackID, duration uint32, isVideo bool) []byte {
	buf := make([]byte, 80)
	putU32(buf[0:4], 0) // creation time
	putU32(buf[4:8], 0) // modification time
	putU32(buf[8:12], trackID)
	// reserved 4 bytes
	putU32(buf[16:20], duration)
	// reserved 8 bytes + layer, alt group, volume
	if !isVideo {
		putU16(buf[32:34], 0x0100) // volume
	}
	// reserved 2 bytes + matrix (36 bytes)
	// identity matrix
	putU32(buf[36:40], 0x00010000)
	putU32(buf[48:52], 0x00010000)
	putU32(buf[60:64], 0x40000000)

	if isVideo {
		putU32(buf[64:68], 1920<<16) // width
		putU32(buf[68:72], 1080<<16) // height
	}

	return buf[:72]
}

func (m *Muxer) buildMdia(isVideo bool, timescale uint32, samples []sampleEntry) []byte {
	var buf bytes.Buffer

	dur := m.totalDuration(samples)
	mdhd := make([]byte, 24)
	putU32(mdhd[8:12], timescale)
	putU32(mdhd[12:16], clampUint64ToUint32(dur))
	putU32(mdhd[16:20], 0x55C40000) // und language
	writeFullBox(&buf, [4]byte{'m', 'd', 'h', 'd'}, 0, 0, mdhd)

	hdlr := m.buildHdlr(isVideo)
	writeFullBox(&buf, [4]byte{'h', 'd', 'l', 'r'}, 0, 0, hdlr)

	minf := m.buildMinf(isVideo, samples)
	writeBox(&buf, [4]byte{'m', 'i', 'n', 'f'}, minf)

	return buf.Bytes()
}

func (m *Muxer) buildHdlr(isVideo bool) []byte {
	var buf bytes.Buffer
	buf.Write(make([]byte, 4)) // pre-defined
	if isVideo {
		buf.Write([]byte("vide"))
	} else {
		buf.Write([]byte("soun"))
	}
	buf.Write(make([]byte, 12)) // reserved
	if isVideo {
		buf.Write([]byte("VideoHandler\x00"))
	} else {
		buf.Write([]byte("SoundHandler\x00"))
	}
	return buf.Bytes()
}

func (m *Muxer) buildMinf(isVideo bool, samples []sampleEntry) []byte {
	var buf bytes.Buffer

	if isVideo {
		vmhd := make([]byte, 8)
		writeFullBox(&buf, [4]byte{'v', 'm', 'h', 'd'}, 0, 1, vmhd)
	} else {
		smhd := make([]byte, 4)
		writeFullBox(&buf, [4]byte{'s', 'm', 'h', 'd'}, 0, 0, smhd)
	}

	// dinf + dref (data reference: url self-contained)
	var dref bytes.Buffer
	putU32Buf(&dref, 1) // entry count
	writeFullBox(&dref, [4]byte{'u', 'r', 'l', ' '}, 0, 1, nil)
	var dinf bytes.Buffer
	writeFullBox(&dinf, [4]byte{'d', 'r', 'e', 'f'}, 0, 0, dref.Bytes())
	writeBox(&buf, [4]byte{'d', 'i', 'n', 'f'}, dinf.Bytes())

	stbl := m.buildStbl(isVideo, samples)
	writeBox(&buf, [4]byte{'s', 't', 'b', 'l'}, stbl)

	return buf.Bytes()
}

func (m *Muxer) buildStbl(isVideo bool, samples []sampleEntry) []byte {
	var buf bytes.Buffer

	stsd := m.buildStsd(isVideo)
	writeFullBox(&buf, [4]byte{'s', 't', 's', 'd'}, 0, 0, stsd)

	stts := buildStts(samples)
	writeFullBox(&buf, [4]byte{'s', 't', 't', 's'}, 0, 0, stts)

	if isVideo {
		ctts, cttsVersion := buildCtts(samples)
		if ctts != nil {
			writeFullBox(&buf, [4]byte{'c', 't', 't', 's'}, cttsVersion, 0, ctts)
		}

		stss := buildStss(samples)
		if stss != nil {
			writeFullBox(&buf, [4]byte{'s', 't', 's', 's'}, 0, 0, stss)
		}
	}

	stsc := buildStsc(len(samples))
	writeFullBox(&buf, [4]byte{'s', 't', 's', 'c'}, 0, 0, stsc)

	stsz := buildStsz(samples)
	writeFullBox(&buf, [4]byte{'s', 't', 's', 'z'}, 0, 0, stsz)

	stco := buildStco(samples)
	writeFullBox(&buf, [4]byte{'s', 't', 'c', 'o'}, 0, 0, stco)

	return buf.Bytes()
}

func (m *Muxer) buildStsd(isVideo bool) []byte {
	var buf bytes.Buffer
	putU32Buf(&buf, 1) // entry count

	if isVideo && m.videoCodec == avframe.CodecH264 {
		buf.Write(m.buildAvc1())
	} else if !isVideo && m.audioCodec == avframe.CodecAAC {
		buf.Write(m.buildMp4a())
	}

	return buf.Bytes()
}

func (m *Muxer) buildAvc1() []byte {
	var buf bytes.Buffer
	// SampleEntry header (78 bytes for video)
	hdr := make([]byte, 78)
	// data_reference_index = 1
	putU16(hdr[6:8], 1)
	// width/height at offset 24/26
	putU16(hdr[24:26], 1920)
	putU16(hdr[26:28], 1080)
	// horiz/vert resolution
	putU32(hdr[28:32], 0x00480000) // 72 dpi
	putU32(hdr[32:36], 0x00480000)
	// frame count
	putU16(hdr[42:44], 1)
	// depth
	putU16(hdr[74:76], 0x0018)

	hdr[76] = 0xFF
	hdr[77] = 0xFF

	buf.Write(hdr)

	// avcC box
	if m.videoSeqHeader != nil {
		writeBox(&buf, [4]byte{'a', 'v', 'c', 'C'}, m.videoSeqHeader)
	}

	data := buf.Bytes()
	var out bytes.Buffer
	size := uint32(8 + len(data))
	var boxHdr [8]byte
	binary.BigEndian.PutUint32(boxHdr[0:4], size)
	copy(boxHdr[4:8], []byte("avc1"))
	out.Write(boxHdr[:])
	out.Write(data)
	return out.Bytes()
}

func (m *Muxer) buildMp4a() []byte {
	var buf bytes.Buffer
	// SampleEntry header (28 bytes for audio)
	hdr := make([]byte, 28)
	putU16(hdr[6:8], 1) // data_reference_index
	putU16(hdr[16:18], m.audioChannels)
	putU16(hdr[18:20], 16) // sample size bits
	putU32(hdr[24:28], m.audioSampleRate<<16)

	buf.Write(hdr)

	// esds box
	esds := buildEsds(m.audioSeqHeader, m.audioSampleRate)
	writeFullBox(&buf, [4]byte{'e', 's', 'd', 's'}, 0, 0, esds)

	data := buf.Bytes()
	var out bytes.Buffer
	size := uint32(8 + len(data))
	var boxHdr [8]byte
	binary.BigEndian.PutUint32(boxHdr[0:4], size)
	copy(boxHdr[4:8], []byte("mp4a"))
	out.Write(boxHdr[:])
	out.Write(data)
	return out.Bytes()
}

func buildEsds(asc []byte, sampleRate uint32) []byte {
	var buf bytes.Buffer

	// ES_Descriptor
	ascLen := len(asc)
	decConfigLen := 13 + 1 + descriptorLengthWidth(ascLen) + ascLen
	esLen := 3 + 1 + descriptorLengthWidth(decConfigLen) + decConfigLen + 3

	buf.WriteByte(0x03) // ES_DescrTag
	writeDescriptorLength(&buf, esLen)
	putU16Buf(&buf, 1) // ES_ID
	buf.WriteByte(0)   // flags

	// DecoderConfigDescriptor
	buf.WriteByte(0x04) // DecoderConfigDescrTag
	writeDescriptorLength(&buf, decConfigLen)
	buf.WriteByte(0x40)                 // objectTypeIndication (AAC)
	buf.WriteByte(0x15)                 // streamType (audio)
	buf.Write([]byte{0x00, 0x00, 0x00}) // bufferSizeDB
	putU32Buf(&buf, 0)                  // maxBitrate
	putU32Buf(&buf, 0)                  // avgBitrate

	// DecoderSpecificInfo
	buf.WriteByte(0x05)
	writeDescriptorLength(&buf, ascLen)
	buf.Write(asc)

	// SLConfigDescriptor
	buf.WriteByte(0x06)
	buf.WriteByte(1)
	buf.WriteByte(0x02)

	return buf.Bytes()
}

func scaleDurationMillis(milliseconds int64, timescale uint32) uint32 {
	if milliseconds <= 0 || timescale == 0 {
		return 0
	}
	maxMillis := int64(math.MaxUint32) * 1000 / int64(timescale)
	if milliseconds > maxMillis {
		return math.MaxUint32
	}
	return uint32(milliseconds * int64(timescale) / 1000) //nolint:gosec // bounded by maxMillis
}

func scaleCompositionOffsetMillis(milliseconds int64, timescale uint32) int32 {
	if timescale == 0 {
		return clampInt64ToInt32(milliseconds)
	}
	maxMillis := int64(math.MaxInt32) * 1000 / int64(timescale)
	minMillis := int64(math.MinInt32) * 1000 / int64(timescale)
	if milliseconds > maxMillis {
		return math.MaxInt32
	}
	if milliseconds < minMillis {
		return math.MinInt32
	}
	return int32(milliseconds * int64(timescale) / 1000) //nolint:gosec // bounded above
}

func clampInt64ToInt32(value int64) int32 {
	if value > math.MaxInt32 {
		return math.MaxInt32
	}
	if value < math.MinInt32 {
		return math.MinInt32
	}
	return int32(value) //nolint:gosec // bounded above
}

func clampUint64ToUint32(value uint64) uint32 {
	if value > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(value) //nolint:gosec // bounded above
}

func scaleDurationUnits(value uint64, sourceTimescale, targetTimescale uint32) uint32 {
	if value == 0 || sourceTimescale == 0 || targetTimescale == 0 {
		return 0
	}
	source := uint64(sourceTimescale)
	target := uint64(targetTimescale)
	whole := value / source
	if whole > uint64(math.MaxUint32)/target {
		return math.MaxUint32
	}
	scaled := whole * target
	fraction := (value % source) * target / source
	if scaled > uint64(math.MaxUint32)-fraction {
		return math.MaxUint32
	}
	return uint32(scaled + fraction) //nolint:gosec // bounded above
}

func descriptorLengthWidth(value int) int {
	width := 1
	for value >= 1<<7 && width < 4 {
		value >>= 7
		width++
	}
	return width
}

func writeDescriptorLength(buf *bytes.Buffer, value int) {
	width := descriptorLengthWidth(value)
	for shift := (width - 1) * 7; shift >= 0; shift -= 7 {
		encoded := byte((value >> shift) & 0x7f) //nolint:gosec // masked to seven bits
		if shift > 0 {
			encoded |= 0x80
		}
		buf.WriteByte(encoded)
	}
}

func buildStts(samples []sampleEntry) []byte {
	if len(samples) == 0 {
		buf := make([]byte, 4)
		return buf
	}

	type sttsEntry struct {
		count    uint32
		duration uint32
	}

	var entries []sttsEntry
	for _, s := range samples {
		if len(entries) > 0 && entries[len(entries)-1].duration == s.duration {
			entries[len(entries)-1].count++
		} else {
			entries = append(entries, sttsEntry{count: 1, duration: s.duration})
		}
	}

	buf := make([]byte, 4+len(entries)*8)
	putU32(buf[0:4], uint32(len(entries)))
	for i, e := range entries {
		off := 4 + i*8
		putU32(buf[off:off+4], e.count)
		putU32(buf[off+4:off+8], e.duration)
	}
	return buf
}

func buildCtts(samples []sampleEntry) ([]byte, uint8) {
	hasCTS := false
	version := uint8(0)
	for _, s := range samples {
		if s.cts != 0 {
			hasCTS = true
		}
		if s.cts < 0 {
			version = 1
		}
	}
	if !hasCTS {
		return nil, 0
	}

	type cttsEntry struct {
		count  uint32
		offset int32
	}

	var entries []cttsEntry
	for _, s := range samples {
		if len(entries) > 0 && entries[len(entries)-1].offset == s.cts {
			entries[len(entries)-1].count++
		} else {
			entries = append(entries, cttsEntry{count: 1, offset: s.cts})
		}
	}

	buf := make([]byte, 4+len(entries)*8)
	putU32(buf[0:4], uint32(len(entries)))
	for i, e := range entries {
		off := 4 + i*8
		putU32(buf[off:off+4], e.count)
		putU32(buf[off+4:off+8], uint32(e.offset))
	}
	return buf, version
}

func buildStss(samples []sampleEntry) []byte {
	var syncIndices []uint32
	for i, s := range samples {
		if s.isSync {
			syncIndices = append(syncIndices, uint32(i+1))
		}
	}
	if len(syncIndices) == len(samples) {
		return nil // all sync, no need for stss
	}

	buf := make([]byte, 4+len(syncIndices)*4)
	putU32(buf[0:4], uint32(len(syncIndices)))
	for i, idx := range syncIndices {
		putU32(buf[4+i*4:8+i*4], idx)
	}
	return buf
}

func buildStsc(sampleCount int) []byte {
	// One chunk per sample (simplest approach)
	buf := make([]byte, 4+12)
	putU32(buf[0:4], 1)   // entry count
	putU32(buf[4:8], 1)   // first chunk
	putU32(buf[8:12], 1)  // samples per chunk
	putU32(buf[12:16], 1) // sample description index
	return buf
}

func buildStsz(samples []sampleEntry) []byte {
	buf := make([]byte, 8+len(samples)*4)
	putU32(buf[0:4], 0) // sample_size = 0 (variable)
	putU32(buf[4:8], uint32(len(samples)))
	for i, s := range samples {
		putU32(buf[8+i*4:12+i*4], s.size)
	}
	return buf
}

func buildStco(samples []sampleEntry) []byte {
	buf := make([]byte, 4+len(samples)*4)
	putU32(buf[0:4], uint32(len(samples)))
	for i, s := range samples {
		putU32(buf[4+i*4:8+i*4], uint32(s.offset))
	}
	return buf
}

func putU32Buf(buf *bytes.Buffer, v uint32) {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	buf.Write(b[:])
}

func putU16Buf(buf *bytes.Buffer, v uint16) {
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], v)
	buf.Write(b[:])
}
