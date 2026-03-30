package httpstream

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/codec/aac"
)

// GenerateMPD generates a DASH MPD manifest for the current segment state.
func (d *DASHManager) GenerateMPD() string {
	d.mu.RLock()
	defer d.mu.RUnlock()

	segs := d.videoSegments
	audioSegs := d.audioSegments

	var totalDur float64
	for _, seg := range segs {
		totalDur += seg.Duration
	}

	bufferDepth := totalDur
	if bufferDepth < d.targetDur*3 {
		bufferDepth = d.targetDur * 3
	}

	// Compute average segment duration for minimumUpdatePeriod.
	avgDurMs := int(d.targetDur * 1000)
	if len(segs) > 2 {
		var stableDur float64
		for _, seg := range segs[2:] {
			stableDur += seg.Duration
		}
		avgDurMs = int(math.Round(stableDur / float64(len(segs)-2) * 1000))
	} else if len(segs) > 0 {
		avgDurMs = int(math.Round(segs[len(segs)-1].Duration * 1000))
	}
	if avgDurMs <= 0 {
		avgDurMs = int(d.targetDur * 1000)
	}

	// startNumber corresponds to the first segment in the current window.
	startNumber := 1
	if len(segs) > 0 {
		startNumber = segs[0].SeqNum + 1 // SeqNum is 0-based; URL numbers are 1-based
	}

	ast := d.startTime

	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	fmt.Fprintf(&sb, `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="dynamic" minimumUpdatePeriod="PT%dS" availabilityStartTime="%s" publishTime="%s" timeShiftBufferDepth="PT%dS" minBufferTime="PT2S" profiles="urn:mpeg:dash:profile:isoff-live:2011">`,
		int(math.Ceil(float64(avgDurMs)/1000.0)),
		ast.Format(time.RFC3339),
		time.Now().UTC().Format(time.RFC3339),
		int(math.Ceil(bufferDepth)),
	)
	sb.WriteString("\n")
	// UTCTiming: embed the current server time directly so dash.js can
	// synchronize its clock without a separate round-trip.
	fmt.Fprintf(&sb, "  <UTCTiming schemeIdUri=\"urn:mpeg:dash:utc:direct:2014\" value=\"%s\"/>\n",
		time.Now().UTC().Format("2006-01-02T15:04:05.000Z"))
	sb.WriteString(`  <Period id="0" start="PT0S">` + "\n")

	// Video AdaptationSet with SegmentTimeline.
	sb.WriteString(`    <AdaptationSet id="0" contentType="video" mimeType="video/mp4" startWithSAP="1" segmentAlignment="true">` + "\n")
	fmt.Fprintf(&sb, `      <SegmentTemplate timescale="1000" startNumber="%d" initialization="%s/vinit.mp4" media="%s/v$Number$.m4s">`,
		startNumber, d.basePath, d.basePath)
	sb.WriteString("\n")
	sb.WriteString("        <SegmentTimeline>\n")
	timeMs := int64(math.Round(d.timeBase * 1000))
	for i, seg := range segs {
		durMs := int64(math.Round(seg.Duration * 1000))
		if i == 0 {
			fmt.Fprintf(&sb, "          <S t=\"%d\" d=\"%d\"/>\n", timeMs, durMs)
		} else {
			fmt.Fprintf(&sb, "          <S d=\"%d\"/>\n", durMs)
		}
		timeMs += durMs
	}
	sb.WriteString("        </SegmentTimeline>\n")
	sb.WriteString("      </SegmentTemplate>\n")
	videoCodecStr := d.videoCodecStr
	if videoCodecStr == "" {
		videoCodecStr = "avc1.640028"
	}
	vw, vh := d.videoWidth, d.videoHeight
	if vw <= 0 {
		vw = 1920
	}
	if vh <= 0 {
		vh = 1080
	}
	fmt.Fprintf(&sb, "      <Representation id=\"0\" bandwidth=\"2000000\" codecs=\"%s\" width=\"%d\" height=\"%d\"/>\n",
		videoCodecStr, vw, vh)
	sb.WriteString("    </AdaptationSet>\n")

	// Audio AdaptationSet with SegmentTimeline.
	if d.hasAudio && len(audioSegs) > 0 {
		audioCodecs := d.audioCodec
		if audioCodecs == "" {
			audioCodecs = "mp4a.40.2"
		}
		audioStartNumber := 1
		if len(audioSegs) > 0 {
			audioStartNumber = audioSegs[0].SeqNum + 1
		}
		sb.WriteString(`    <AdaptationSet id="1" contentType="audio" mimeType="audio/mp4" startWithSAP="1" segmentAlignment="true">` + "\n")
		fmt.Fprintf(&sb, `      <SegmentTemplate timescale="1000" startNumber="%d" initialization="%s/audio_init.mp4" media="%s/a$Number$.m4s">`,
			audioStartNumber, d.basePath, d.basePath)
		sb.WriteString("\n")
		sb.WriteString("        <SegmentTimeline>\n")
		audioTimeMs := int64(math.Round(d.timeBase * 1000))
		for i, seg := range audioSegs {
			durMs := int64(math.Round(seg.Duration * 1000))
			if i == 0 {
				fmt.Fprintf(&sb, "          <S t=\"%d\" d=\"%d\"/>\n", audioTimeMs, durMs)
			} else {
				fmt.Fprintf(&sb, "          <S d=\"%d\"/>\n", durMs)
			}
			audioTimeMs += durMs
		}
		sb.WriteString("        </SegmentTimeline>\n")
		sb.WriteString("      </SegmentTemplate>\n")
		asr := d.audioSampleRate
		if asr <= 0 {
			asr = 44100
		}
		fmt.Fprintf(&sb, `      <Representation id="1" bandwidth="128000" codecs="%s" audioSamplingRate="%d"/>`, audioCodecs, asr)
		sb.WriteString("\n")
		sb.WriteString("    </AdaptationSet>\n")
	}

	sb.WriteString("  </Period>\n")
	sb.WriteString("</MPD>\n")

	return sb.String()
}

// SegmentCount returns the number of video segments currently available.
func (d *DASHManager) SegmentCount() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.videoSegments)
}

// SegmentRange returns the SeqNum range [lo, hi] of video segments in memory.
func (d *DASHManager) SegmentRange() (lo, hi int) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if len(d.videoSegments) == 0 {
		return -1, -1
	}
	return d.videoSegments[0].SeqNum, d.videoSegments[len(d.videoSegments)-1].SeqNum
}

// dashVideoCodecString returns the DASH codecs string (e.g., "avc1.640028")
// extracted from the video sequence header. Falls back to a sensible default
// if the header is unavailable or the codec is not recognized.
func dashVideoCodecString(codec avframe.CodecType, seqHeader *avframe.AVFrame) string {
	if seqHeader != nil && codec == avframe.CodecH264 && len(seqHeader.Payload) >= 4 {
		// AVCDecoderConfigurationRecord: [1]=profile, [2]=constraint, [3]=level
		return fmt.Sprintf("avc1.%02x%02x%02x",
			seqHeader.Payload[1], seqHeader.Payload[2], seqHeader.Payload[3])
	}
	switch codec {
	case avframe.CodecH264:
		return "avc1.640028" // fallback: High profile, level 4.0
	case avframe.CodecH265:
		return "hvc1.1.6.L120.B0" // fallback: Main profile, level 4.0
	default:
		return "avc1.640028"
	}
}

// dashAudioCodecString returns the DASH codecs string for an audio codec.
// For AAC it parses the AudioSpecificConfig to extract the actual audioObjectType
// so the codec string (e.g. "mp4a.40.2" for AAC-LC, "mp4a.40.5" for HE-AAC)
// matches the ESDS in the init segment. Chrome MSE validates this match.
func dashAudioCodecString(codec avframe.CodecType, audioSeqHeader *avframe.AVFrame) string {
	switch codec {
	case avframe.CodecAAC:
		aot := 2 // default: AAC-LC
		if audioSeqHeader != nil && len(audioSeqHeader.Payload) >= 2 {
			if info, err := aac.ParseAudioSpecificConfig(audioSeqHeader.Payload); err == nil {
				aot = info.ObjectType
			}
		}
		return fmt.Sprintf("mp4a.40.%d", aot)
	case avframe.CodecOpus:
		return "opus"
	case avframe.CodecMP3:
		return "mp4a.40.34"
	default:
		return "mp4a.40.2"
	}
}
