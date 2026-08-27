//go:build audiocodec

package core

import (
	"testing"
	"time"

	"github.com/im-pingo/liveforge/pkg/avframe"
)

// Catches a codec-boundary return closing the producer-owned ring after one
// HTTP-style consumer hands off and releases its shared transform subscription.
func TestTranscodeManagerHTTPHandoffDoesNotCloseSharedAudioReader(t *testing.T) {
	stream := newTranscodeTestStream(avframe.CodecG711U)
	start := stream.RingBuffer().WriteCursor()
	httpReader, releaseHTTP, err := stream.TranscodeManager().GetOrCreateAudioReaderAt(avframe.CodecAAC, start)
	if err != nil {
		t.Fatal(err)
	}
	peerReader, releasePeer, err := stream.TranscodeManager().GetOrCreateAudioReaderAt(avframe.CodecAAC, start)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		httpReader.Close()
		peerReader.Close()
		releaseHTTP()
		releasePeer()
		stream.RingBuffer().Close()
	})

	// The HTTP worker changes owner at this header and releases only its reader.
	releaseHTTP()
	httpReader.Close()
	directHeader := avframe.NewAVFrame(
		avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeSequenceHeader,
		100, 100, []byte{0x12, 0x10},
	)
	directMedia := avframe.NewAVFrame(
		avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe,
		120, 120, []byte{0x21, 0x10},
	)
	stream.WriteFrame(directHeader)
	stream.WriteFrame(directMedia)

	deadline := time.After(time.Second)
	for {
		result := make(chan struct {
			frame *avframe.AVFrame
			ok    bool
		}, 1)
		go func() {
			frame, ok := peerReader.Read()
			result <- struct {
				frame *avframe.AVFrame
				ok    bool
			}{frame: frame, ok: ok}
		}()
		select {
		case got := <-result:
			if !got.ok {
				t.Fatal("shared transform producer closed the peer reader at the AAC handoff")
			}
			if got.frame == directMedia {
				return
			}
		case <-deadline:
			peerReader.Close()
			t.Fatal("peer reader did not receive direct AAC after the HTTP handoff")
		}
	}
}
