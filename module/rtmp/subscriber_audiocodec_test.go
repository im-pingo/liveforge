//go:build audiocodec

package rtmp

import (
	"bytes"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/audiocodec"
	"github.com/im-pingo/liveforge/pkg/avframe"
)

func TestRTMPTranscodeUsesLiveCursorForDirectVideo(t *testing.T) {
	stream := newRTMPStartupStream(t, &avframe.MediaInfo{
		VideoCodec: avframe.CodecH264,
		AudioCodec: avframe.CodecG711U,
		SampleRate: 8000,
		Channels:   1,
	})
	core.SetTranscodeManagerForTest(stream, core.NewTranscodeManager(stream, audiocodec.Global(), 32))
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader,
		0, 0, []byte{1, 0x64},
	))
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe,
		4000, 4000, []byte{0, 0, 0, 2, 0x65, 1},
	))

	_, conn, reader, _ := startRTMPStartupSubscriber(t, stream)
	videoHeaderCount := 0
	audioHeaderCount := 0
	for range 2 {
		msg := readRTMPStartupMessage(t, conn, reader)
		if msg.TypeID == MsgVideo && msg.Timestamp == 0 {
			videoHeaderCount++
		}
		if msg.TypeID == MsgAudio && msg.Timestamp == 0 {
			audioHeaderCount++
		}
	}
	if videoHeaderCount != 1 {
		t.Fatalf("snapshotted video sequence header delivered %d times before live media, want once", videoHeaderCount)
	}
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe,
		5000, 5000, []byte{0, 0, 0, 2, 0x41, 2},
	))

	cachedVideoCount := 1
	liveVideoSeen := false
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		msg := readRTMPStartupMessage(t, conn, reader)
		if msg.TypeID == MsgAudio && msg.Timestamp == 0 {
			audioHeaderCount++
			if liveVideoSeen {
				break
			}
		}
		if msg.TypeID != MsgVideo {
			continue
		}
		switch msg.Timestamp {
		case 0:
			videoHeaderCount++
		case 4000:
			cachedVideoCount++
		case 5000:
			liveVideoSeen = true
			if cachedVideoCount != 1 {
				t.Fatalf("cached video DTS 4000 delivered %d times, want once", cachedVideoCount)
			}
			if videoHeaderCount != 1 {
				t.Fatalf("snapshotted video sequence header delivered %d times, want once", videoHeaderCount)
			}
			if audioHeaderCount == 1 {
				return
			}
		}
	}
	if !liveVideoSeen {
		t.Fatal("live video DTS 5000 was not delivered")
	}
	if audioHeaderCount != 1 {
		t.Fatalf("target audio sequence header delivered %d times, want once", audioHeaderCount)
	}
}

// Catches a codec-boundary return in the shared transform producer closing the
// audio reader and making writeTranscodedLoop terminate its independent video path.
func TestRTMPTranscodeCodecSwitchKeepsDirectVideoPlaying(t *testing.T) {
	stream := newRTMPStartupStream(t, &avframe.MediaInfo{
		VideoCodec: avframe.CodecH264,
		AudioCodec: avframe.CodecG711U,
		SampleRate: 8000,
		Channels:   1,
	})
	core.SetTranscodeManagerForTest(stream, core.NewTranscodeManager(stream, audiocodec.Global(), 64))
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader,
		0, 0, []byte{1, 0x64},
	))
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe,
		0, 0, []byte{0, 0, 0, 2, 0x65, 1},
	))

	_, conn, reader, done := startRTMPStartupSubscriber(t, stream)
	for range 2 {
		readRTMPStartupMessage(t, conn, reader)
	}
	for i := range 4 {
		dts := int64(20 + i*20)
		stream.WriteFrame(avframe.NewAVFrame(
			avframe.MediaTypeAudio, avframe.CodecG711U, avframe.FrameTypeInterframe,
			dts, dts, bytes.Repeat([]byte{0xff}, 160),
		))
	}
	transcodedAudioSeen := false
	for range 8 {
		msg := readRTMPStartupMessage(t, conn, reader)
		if msg.TypeID == MsgAudio && msg.Timestamp > 0 {
			transcodedAudioSeen = true
			break
		}
	}
	if !transcodedAudioSeen {
		t.Fatal("RTMP did not start the G.711-to-AAC transform before the codec switch")
	}

	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeSequenceHeader,
		100, 100, []byte{0x12, 0x10},
	))
	select {
	case <-done:
		t.Fatal("RTMP playback stopped when the shared transform reached the AAC codec boundary")
	case <-time.After(100 * time.Millisecond):
	}

	encoder := audiocodec.NewFFmpegEncoder("aac", 44100, 2)
	defer encoder.Close()
	directAAC, err := encoder.Encode(&audiocodec.PCMFrame{
		Samples: make([]int16, 1024*2), SampleRate: 44100, Channels: 2,
	})
	if err != nil {
		t.Fatalf("encode direct AAC: %v", err)
	}
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe,
		120, 120, directAAC,
	))
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe,
		500, 500, []byte{0, 0, 0, 2, 0x41, 0x7e},
	))

	directAudioSeen := false
	directVideoSeen := false
	for range 12 {
		msg := readRTMPStartupMessage(t, conn, reader)
		if msg.TypeID == MsgAudio && msg.Timestamp == 120 {
			directAudioSeen = true
		}
		if msg.TypeID == MsgVideo && msg.Timestamp == 500 {
			directVideoSeen = true
		}
		if directAudioSeen && directVideoSeen {
			break
		}
	}
	if !directAudioSeen || !directVideoSeen {
		t.Fatalf("post-switch RTMP media audio/video = %v/%v, want both", directAudioSeen, directVideoSeen)
	}
}
