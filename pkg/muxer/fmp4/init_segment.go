package fmp4

import (
	"bytes"
	"encoding/binary"

	"github.com/im-pingo/liveforge/pkg/avframe"
)

const (
	videoTrackID = 1
	audioTrackID = 2
	timescaleVideo = 90000
	timescaleAudio = 44100 // overridden by actual sample rate
)

// BuildInitSegment generates the ftyp+moov init segment for fragmented MP4.
func BuildInitSegment(videoCodec, audioCodec avframe.CodecType, videoSeqHeader, audioSeqHeader []byte, width, height, sampleRate, channels int) []byte {
	var buf bytes.Buffer

	// ftyp box
	writeFtyp(&buf)

	// moov box
	var moov bytes.Buffer
	writeMvhd(&moov)

	if videoCodec != 0 {
		writeVideoTrak(&moov, videoCodec, videoSeqHeader, width, height)
	}
	if audioCodec != 0 {
		audioTS := sampleRate
		if audioTS == 0 {
			audioTS = timescaleAudio
		}
		writeAudioTrak(&moov, audioCodec, audioSeqHeader, audioTS, channels)
	}

	writeMvex(&moov, videoCodec != 0, audioCodec != 0)

	WriteBox(&buf, BoxMoov, moov.Bytes())
	return buf.Bytes()
}

func writeFtyp(w *bytes.Buffer) {
	var payload bytes.Buffer
	payload.Write([]byte("isom"))    // major_brand
	binary.Write(&payload, binary.BigEndian, uint32(0x200)) // minor_version
	payload.Write([]byte("isom"))    // compatible_brand
	payload.Write([]byte("iso6"))
	payload.Write([]byte("mp41"))
	WriteBox(w, BoxFtyp, payload.Bytes())
}

func writeMvhd(w *bytes.Buffer) {
	// mvhd version 0 layout (96 bytes payload after fullbox header):
	//   [0:4]   creation_time
	//   [4:8]   modification_time
	//   [8:12]  timescale
	//   [12:16] duration
	//   [16:20] rate (fixed-point 16.16)
	//   [20:22] volume (fixed-point 8.8)
	//   [22:32] reserved
	//   [32:68] matrix (9 x int32)
	//   [68:92] pre_defined
	//   [92:96] next_track_ID
	payload := make([]byte, 96)
	binary.BigEndian.PutUint32(payload[8:12], timescaleVideo)  // timescale
	binary.BigEndian.PutUint32(payload[12:16], 0)              // duration
	binary.BigEndian.PutUint32(payload[16:20], 0x00010000)     // rate = 1.0
	binary.BigEndian.PutUint16(payload[20:22], 0x0100)         // volume = 1.0
	// matrix (identity): [0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000]
	binary.BigEndian.PutUint32(payload[32:36], 0x00010000)
	binary.BigEndian.PutUint32(payload[48:52], 0x00010000)
	binary.BigEndian.PutUint32(payload[64:68], 0x40000000)
	binary.BigEndian.PutUint32(payload[92:96], 3) // next_track_ID
	WriteFullBox(w, BoxMvhd, 0, 0, payload)
}

func writeVideoTrak(w *bytes.Buffer, codec avframe.CodecType, seqHeader []byte, width, height int) {
	var trak bytes.Buffer

	// tkhd version 0 layout (80 bytes payload after fullbox header):
	//   [0:4]   creation_time
	//   [4:8]   modification_time
	//   [8:12]  track_ID
	//   [12:16] reserved
	//   [16:20] duration
	//   [20:28] reserved
	//   [28:30] layer
	//   [30:32] alternate_group
	//   [32:34] volume
	//   [34:36] reserved
	//   [36:72] matrix (9 x int32)
	//   [72:76] width (fixed-point 16.16)
	//   [76:80] height (fixed-point 16.16)
	tkhd := make([]byte, 80)
	binary.BigEndian.PutUint32(tkhd[8:12], videoTrackID)
	// matrix identity
	binary.BigEndian.PutUint32(tkhd[36:40], 0x00010000)
	binary.BigEndian.PutUint32(tkhd[52:56], 0x00010000)
	binary.BigEndian.PutUint32(tkhd[68:72], 0x40000000)
	binary.BigEndian.PutUint32(tkhd[72:76], uint32(width)<<16)
	binary.BigEndian.PutUint32(tkhd[76:80], uint32(height)<<16)
	WriteFullBox(&trak, BoxTkhd, 0, 0x000003, tkhd) // flags: track_enabled | track_in_movie

	// mdia
	var mdia bytes.Buffer
	mdhd := make([]byte, 20) // version 0
	binary.BigEndian.PutUint32(mdhd[8:12], timescaleVideo) // timescale
	WriteFullBox(&mdia, BoxMdhd, 0, 0, mdhd)

	// hdlr
	var hdlr bytes.Buffer
	hdlr.Write(make([]byte, 4)) // pre_defined
	hdlr.Write([]byte("vide"))
	hdlr.Write(make([]byte, 12)) // reserved
	hdlr.Write([]byte("VideoHandler\x00"))
	WriteFullBox(&mdia, BoxHdlr, 0, 0, hdlr.Bytes())

	// minf
	var minf bytes.Buffer
	WriteFullBox(&minf, BoxVmhd, 0, 1, make([]byte, 8))
	writeDinf(&minf)
	writeVideoStbl(&minf, codec, seqHeader, width, height)
	WriteBox(&mdia, BoxMinf, minf.Bytes())

	WriteBox(&trak, BoxMdia, mdia.Bytes())
	WriteBox(w, BoxTrak, trak.Bytes())
}

func writeAudioTrak(w *bytes.Buffer, codec avframe.CodecType, seqHeader []byte, sampleRate, channels int) {
	var trak bytes.Buffer

	// tkhd (same layout as video tkhd)
	tkhd := make([]byte, 80)
	binary.BigEndian.PutUint32(tkhd[8:12], audioTrackID)
	binary.BigEndian.PutUint16(tkhd[32:34], 0x0100) // volume = 1.0
	// matrix identity
	binary.BigEndian.PutUint32(tkhd[36:40], 0x00010000)
	binary.BigEndian.PutUint32(tkhd[52:56], 0x00010000)
	binary.BigEndian.PutUint32(tkhd[68:72], 0x40000000)
	WriteFullBox(&trak, BoxTkhd, 0, 0x000003, tkhd)

	// mdia
	var mdia bytes.Buffer
	mdhd := make([]byte, 20)
	binary.BigEndian.PutUint32(mdhd[8:12], uint32(sampleRate)) // timescale = sample rate
	WriteFullBox(&mdia, BoxMdhd, 0, 0, mdhd)

	var hdlr bytes.Buffer
	hdlr.Write(make([]byte, 4))
	hdlr.Write([]byte("soun"))
	hdlr.Write(make([]byte, 12))
	hdlr.Write([]byte("SoundHandler\x00"))
	WriteFullBox(&mdia, BoxHdlr, 0, 0, hdlr.Bytes())

	var minf bytes.Buffer
	WriteFullBox(&minf, BoxSmhd, 0, 0, make([]byte, 4))
	writeDinf(&minf)
	writeAudioStbl(&minf, codec, seqHeader, sampleRate, channels)
	WriteBox(&mdia, BoxMinf, minf.Bytes())

	WriteBox(&trak, BoxMdia, mdia.Bytes())
	WriteBox(w, BoxTrak, trak.Bytes())
}

func writeDinf(w *bytes.Buffer) {
	var dinf bytes.Buffer
	// dref with one url entry (self-contained)
	var drefPayload bytes.Buffer
	binary.Write(&drefPayload, binary.BigEndian, uint32(1)) // entry_count
	WriteFullBox(&drefPayload, BoxUrl, 0, 0x000001, nil)    // flags=1 (self-contained)
	WriteFullBox(&dinf, BoxDref, 0, 0, drefPayload.Bytes())
	WriteBox(w, BoxDinf, dinf.Bytes())
}

func writeVideoStbl(w *bytes.Buffer, codec avframe.CodecType, seqHeader []byte, width, height int) {
	var stbl bytes.Buffer

	// stsd
	var stsdPayload bytes.Buffer
	binary.Write(&stsdPayload, binary.BigEndian, uint32(1)) // entry_count
	writeVideoSampleEntry(&stsdPayload, codec, seqHeader, width, height)
	WriteFullBox(&stbl, BoxStsd, 0, 0, stsdPayload.Bytes())

	// Empty required boxes
	WriteFullBox(&stbl, BoxStts, 0, 0, make([]byte, 4)) // entry_count=0
	WriteFullBox(&stbl, BoxStsc, 0, 0, make([]byte, 4))
	WriteFullBox(&stbl, BoxStsz, 0, 0, make([]byte, 8)) // sample_size=0, sample_count=0
	WriteFullBox(&stbl, BoxStco, 0, 0, make([]byte, 4))

	WriteBox(w, BoxStbl, stbl.Bytes())
}

func writeAudioStbl(w *bytes.Buffer, codec avframe.CodecType, seqHeader []byte, sampleRate, channels int) {
	var stbl bytes.Buffer

	var stsdPayload bytes.Buffer
	binary.Write(&stsdPayload, binary.BigEndian, uint32(1))
	writeAudioSampleEntry(&stsdPayload, codec, seqHeader, sampleRate, channels)
	WriteFullBox(&stbl, BoxStsd, 0, 0, stsdPayload.Bytes())

	WriteFullBox(&stbl, BoxStts, 0, 0, make([]byte, 4))
	WriteFullBox(&stbl, BoxStsc, 0, 0, make([]byte, 4))
	WriteFullBox(&stbl, BoxStsz, 0, 0, make([]byte, 8))
	WriteFullBox(&stbl, BoxStco, 0, 0, make([]byte, 4))

	WriteBox(w, BoxStbl, stbl.Bytes())
}

func writeVideoSampleEntry(w *bytes.Buffer, codec avframe.CodecType, seqHeader []byte, width, height int) {
	// Visual sample entry: 6 reserved + 2 data_ref_index + 16 pre_defined/reserved + 2 width + 2 height + rest
	entry := make([]byte, 78) // standard visual sample entry
	binary.BigEndian.PutUint16(entry[6:8], 1)    // data_reference_index
	binary.BigEndian.PutUint16(entry[24:26], uint16(width))
	binary.BigEndian.PutUint16(entry[26:28], uint16(height))
	binary.BigEndian.PutUint32(entry[28:32], 0x00480000) // horizresolution = 72 dpi
	binary.BigEndian.PutUint32(entry[32:36], 0x00480000) // vertresolution = 72 dpi
	binary.BigEndian.PutUint16(entry[40:42], 1)          // frame_count = 1
	entry[74] = 0x18 // depth = 24
	binary.BigEndian.PutUint16(entry[76:78], 0xFFFF) // pre_defined = -1

	var box bytes.Buffer
	box.Write(entry)

	// Codec config box
	switch codec {
	case avframe.CodecH264:
		if len(seqHeader) > 0 {
			WriteBox(&box, BoxAvcC, seqHeader)
		}
		WriteBox(w, BoxAvc1, box.Bytes())
	case avframe.CodecH265:
		if len(seqHeader) > 0 {
			WriteBox(&box, BoxHvcC, seqHeader)
		}
		WriteBox(w, BoxHev1, box.Bytes())
	case avframe.CodecAV1:
		if len(seqHeader) > 0 {
			WriteBox(&box, BoxAv1C, seqHeader)
		}
		WriteBox(w, BoxAv01, box.Bytes())
	case avframe.CodecVP8:
		if len(seqHeader) > 0 {
			WriteBox(&box, BoxVpcC, seqHeader)
		}
		WriteBox(w, BoxVp08, box.Bytes())
	case avframe.CodecVP9:
		if len(seqHeader) > 0 {
			WriteBox(&box, BoxVpcC, seqHeader)
		}
		WriteBox(w, BoxVp09, box.Bytes())
	default:
		WriteBox(w, BoxAvc1, box.Bytes())
	}
}

func writeAudioSampleEntry(w *bytes.Buffer, codec avframe.CodecType, seqHeader []byte, sampleRate, channels int) {
	// Audio sample entry: 6 reserved + 2 data_ref_index + 8 reserved + 2 channel_count + 2 sample_size + 4 reserved + 4 sample_rate
	entry := make([]byte, 28)
	binary.BigEndian.PutUint16(entry[6:8], 1) // data_reference_index
	binary.BigEndian.PutUint16(entry[16:18], uint16(channels))
	binary.BigEndian.PutUint16(entry[18:20], 16) // sample_size = 16 bits
	binary.BigEndian.PutUint32(entry[24:28], uint32(sampleRate)<<16)

	var box bytes.Buffer
	box.Write(entry)

	switch codec {
	case avframe.CodecAAC:
		if len(seqHeader) > 0 {
			writeEsds(&box, seqHeader)
		}
		WriteBox(w, BoxMp4a, box.Bytes())
	case avframe.CodecMP3:
		WriteBox(w, BoxMp3, box.Bytes())
	case avframe.CodecOpus:
		if len(seqHeader) > 0 {
			WriteBox(&box, BoxDOps, seqHeader)
		}
		WriteBox(w, BoxOpus, box.Bytes())
	default:
		WriteBox(w, BoxMp4a, box.Bytes())
	}
}

func writeEsds(w *bytes.Buffer, audioSpecificConfig []byte) {
	// Simplified ESDS box containing the AudioSpecificConfig
	ascLen := len(audioSpecificConfig)
	payload := make([]byte, 0, 27+ascLen)

	// ES_Descriptor tag=0x03
	esDescLen := 23 + ascLen
	payload = append(payload, 0x03, byte(esDescLen))
	payload = append(payload, 0x00, 0x01) // ES_ID = 1
	payload = append(payload, 0x00)       // stream priority

	// DecoderConfigDescriptor tag=0x04
	decConfLen := 15 + ascLen
	payload = append(payload, 0x04, byte(decConfLen))
	payload = append(payload, 0x40) // objectTypeIndication = AAC
	payload = append(payload, 0x15) // streamType = audio (5<<2 | 1)
	payload = append(payload, 0x00, 0x00, 0x00) // bufferSizeDB
	payload = append(payload, 0x00, 0x01, 0xF4, 0x00) // maxBitrate
	payload = append(payload, 0x00, 0x01, 0xF4, 0x00) // avgBitrate

	// DecoderSpecificInfo tag=0x05
	payload = append(payload, 0x05, byte(ascLen))
	payload = append(payload, audioSpecificConfig...)

	// SLConfigDescriptor tag=0x06
	payload = append(payload, 0x06, 0x01, 0x02)

	WriteFullBox(w, BoxEsds, 0, 0, payload)
}

func writeMvex(w *bytes.Buffer, hasVideo, hasAudio bool) {
	var mvex bytes.Buffer
	if hasVideo {
		trex := make([]byte, 20)
		binary.BigEndian.PutUint32(trex[0:4], videoTrackID)
		binary.BigEndian.PutUint32(trex[4:8], 1)  // default_sample_description_index
		WriteFullBox(&mvex, BoxTrex, 0, 0, trex)
	}
	if hasAudio {
		trex := make([]byte, 20)
		binary.BigEndian.PutUint32(trex[0:4], audioTrackID)
		binary.BigEndian.PutUint32(trex[4:8], 1)
		WriteFullBox(&mvex, BoxTrex, 0, 0, trex)
	}
	WriteBox(w, BoxMvex, mvex.Bytes())
}
