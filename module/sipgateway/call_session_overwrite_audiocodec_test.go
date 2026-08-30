//go:build audiocodec

package sipgateway

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/audiocodec"
	"github.com/im-pingo/liveforge/pkg/avframe"
	lfertp "github.com/im-pingo/liveforge/pkg/rtp"
	"github.com/im-pingo/liveforge/pkg/util"
	pionrtp "github.com/pion/rtp/v2"
)

func TestCallSessionOutboundTranscodedSourceOverwriteKeepsTargetAudioAndGatesVideo(t *testing.T) {
	events := installSIPOverwriteLogObserver(t)
	stream := newSIPOverwriteStream(t, "sip/transcoded-source-overwrite", 8, true)
	snapshot := stream.StartupSnapshot()

	sourceBuffer := util.NewRingBuffer[*avframe.AVFrame](2)
	sourceReader := sourceBuffer.NewReaderAt(0)
	for _, frame := range []*avframe.AVFrame{
		sipVideoFrame(avframe.FrameTypeInterframe, 0xa1),
		sipVideoFrame(avframe.FrameTypeInterframe, 0xa2),
		sipVideoFrame(avframe.FrameTypeInterframe, 0xa3),
		sipVideoFrame(avframe.FrameTypeInterframe, 0xa4),
	} {
		sourceBuffer.Write(frame)
	}
	audioBuffer := util.NewRingBuffer[*avframe.AVFrame](512)
	audioReader := audioBuffer.NewReaderAt(0)
	wantAudio := make([]byte, 256)
	for i := range wantAudio {
		wantAudio[i] = byte(i)
		audioBuffer.Write(sipTargetAudioFrame(wantAudio[i]))
	}

	call, remoteAudio, remoteVideo, audioPacketizer, videoPacketizer := newSIPDualReaderHarness(t, stream)
	sourceReads := make(chan byte, 2)
	call.pumpObserver = &sipMediaPumpObserver{read: func(reader sipMediaReader, result util.RingReadResult[*avframe.AVFrame]) {
		if reader != sipMediaReaderSource || result.Overwritten > 0 || result.Value == nil || len(result.Value.Payload) == 0 {
			return
		}
		marker := result.Value.Payload[len(result.Value.Payload)-1]
		if marker == 0xb0 || marker == 0xb1 {
			sourceReads <- marker
		}
	}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		call.sendTranscodedAudioAndVideo(
			ctx, stream, snapshot, sourceReader, audioReader,
			audioPacketizer, lfertp.NewSession(0, 8000), call.conn, call.rtcpConn, call.remoteAddr,
			videoPacketizer, lfertp.NewSession(96, 90000), call.video,
		)
	}()
	assertSIPOverwriteEvent(t, events, sipOverwriteLogEvent{
		reader: "source", action: "wait_keyframe", overwritten: 2,
	})

	sourceBuffer.Write(sipVideoFrame(avframe.FrameTypeInterframe, 0xb0))
	waitSIPMarkerSignal(t, sourceReads, 0xb0)
	sourceBuffer.Write(sipVideoFrame(avframe.FrameTypeSequenceHeader, 0xb1))
	waitSIPMarkerSignal(t, sourceReads, 0xb1)
	sourceBuffer.Write(sipVideoFrame(avframe.FrameTypeKeyframe, 0xb2))
	assertRTPMarkers(t, remoteVideo, []byte{0xb1, 0xb2})
	sourceBuffer.Write(sipVideoFrame(avframe.FrameTypeInterframe, 0xb3))
	assertRTPMarkers(t, remoteAudio, wantAudio)
	assertRTPMarkers(t, remoteVideo, []byte{0xb3})

	cancel()
	waitSIPSendLoop(t, done)
}

func TestCallSessionOutboundTargetAudioOverwriteKeepsDirectVideoContinuous(t *testing.T) {
	events := installSIPOverwriteLogObserver(t)
	stream := newSIPOverwriteStream(t, "sip/target-audio-overwrite", 8, true)
	snapshot := stream.StartupSnapshot()

	sourceBuffer := util.NewRingBuffer[*avframe.AVFrame](128)
	sourceReader := sourceBuffer.NewReaderAt(0)
	wantVideo := make([]byte, 64)
	for i := range wantVideo {
		wantVideo[i] = byte(i + 1)
		frameType := avframe.FrameTypeInterframe
		if i%8 == 0 {
			frameType = avframe.FrameTypeKeyframe
		}
		sourceBuffer.Write(sipVideoFrame(frameType, wantVideo[i]))
	}
	audioBuffer := util.NewRingBuffer[*avframe.AVFrame](2)
	audioReader := audioBuffer.NewReaderAt(0)
	for _, marker := range []byte{0xa1, 0xa2, 0xa3, 0xa4} {
		audioBuffer.Write(sipTargetAudioFrame(marker))
	}

	call, remoteAudio, remoteVideo, audioPacketizer, videoPacketizer := newSIPDualReaderHarness(t, stream)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		call.sendTranscodedAudioAndVideo(
			ctx, stream, snapshot, sourceReader, audioReader,
			audioPacketizer, lfertp.NewSession(0, 8000), call.conn, call.rtcpConn, call.remoteAddr,
			videoPacketizer, lfertp.NewSession(96, 90000), call.video,
		)
	}()
	assertSIPOverwriteEvent(t, events, sipOverwriteLogEvent{
		reader: "target_audio", action: "continue_audio", overwritten: 2,
	})
	audioBuffer.Write(sipTargetAudioFrame(0xb0))

	assertRTPMarkers(t, remoteVideo, wantVideo)
	assertRTPMarkers(t, remoteAudio, []byte{0xb0})
	cancel()
	waitSIPSendLoop(t, done)
}

func TestCallSessionOutboundSharedTranscodeProducerSourceOverwriteIsNetworkLost(t *testing.T) {
	stream := core.NewStream(
		"sip/transcode-producer-source-overwrite",
		config.StreamConfig{RingBufferSize: 2},
		config.LimitsConfig{},
		core.NewEventBus(),
	)
	t.Cleanup(func() { stream.Close() })
	if err := stream.SetPublisher(&gatewayTestPublisher{id: "transcode-source", info: &avframe.MediaInfo{
		AudioCodec: avframe.CodecG711A, SampleRate: 8000, Channels: 1,
	}}); err != nil {
		t.Fatalf("SetPublisher: %v", err)
	}
	snapshot := stream.StartupSnapshot()
	for _, marker := range []byte{0xa1, 0xa2, 0xa3, 0xa4} {
		stream.WriteFrame(sipAudioFrame(marker))
	}

	manager := core.NewTranscodeManager(stream, audiocodec.Global(), 8)
	core.SetTranscodeManagerForTest(stream, manager)
	targetReader, releaseTarget, err := manager.GetOrCreateAudioReaderAtFromHistory(avframe.CodecG711U, snapshot)
	if err != nil {
		t.Fatalf("GetOrCreateAudioReaderAtFromHistory: %v", err)
	}

	remoteRTP, remoteRTCP, err := listenLabUDPPair()
	if err != nil {
		t.Fatalf("listen remote pair: %v", err)
	}
	t.Cleanup(func() {
		_ = remoteRTP.Close()
		_ = remoteRTCP.Close()
	})
	localRTP, localRTCP, err := listenLabUDPPair()
	if err != nil {
		t.Fatalf("listen local pair: %v", err)
	}
	call := newCallSession(
		"producer-overwrite", stream.Key(),
		negotiatedCodec{Codec: avframe.CodecG711U, PT: 0, ClockRate: 8000, EncodingName: "PCMU"},
		"outbound", localRTP.LocalAddr().(*net.UDPAddr).Port, localRTCP.LocalAddr().(*net.UDPAddr).Port,
	)
	call.configureMediaSockets(localRTP, localRTCP)
	call.stream = stream
	call.startupSnapshot = snapshot
	call.remoteAddr = remoteRTP.LocalAddr().(*net.UDPAddr)
	call.transcodedAudio = targetReader
	var targetReleases atomic.Int32
	call.releaseAudio = func() {
		targetReleases.Add(1)
		releaseTarget()
	}
	releaseSubscriber, err := stream.AddSubscriberForGeneration("sipgateway", snapshot.Generation)
	if err != nil {
		t.Fatalf("AddSubscriberForGeneration: %v", err)
	}
	var subscriberReleases atomic.Int32
	call.releaseSubscriber = func() {
		subscriberReleases.Add(1)
		releaseSubscriber()
	}
	call.state = CallStateActive
	terminal := make(chan CallState, 1)
	call.onTerminate = func(_ *CallSession, state CallState, _ error) { terminal <- state }
	go call.sendLoop()

	select {
	case state := <-terminal:
		if state != CallStateNetworkLost {
			t.Fatalf("terminal state = %s, want network_lost", state)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shared transcode source overwrite did not terminate the SIP call")
	}

	snapshotAfter := call.snapshot()
	if snapshotAfter.State != CallStateNetworkLost {
		t.Fatalf("call state = %s, want network_lost", snapshotAfter.State)
	}
	if !strings.Contains(snapshotAfter.LastError, "target audio ended") || len([]rune(snapshotAfter.LastError)) > terminalErrorLimit {
		t.Fatalf("last_error = %q, want bounded target-audio failure", snapshotAfter.LastError)
	}
	if got := call.rtpPacketsSent.Load(); got != 0 {
		t.Fatalf("RTP packets after producer overwrite = %d, want 0", got)
	}
	call.Close()
	call.Close()
	if got := targetReleases.Load(); got != 1 {
		t.Fatalf("target-audio releases = %d, want 1", got)
	}
	if got := subscriberReleases.Load(); got != 1 {
		t.Fatalf("subscriber releases = %d, want 1", got)
	}
	if got := stream.Subscribers()["sipgateway"]; got != 0 {
		t.Fatalf("SIP subscribers after producer overwrite = %d, want 0", got)
	}
}

func TestCallSessionOutboundTerminalTargetAudioEOFJoinsSourcePump(t *testing.T) {
	stream := newSIPOverwriteStream(t, "sip/terminal-pump-join", 8, true)
	snapshot := stream.StartupSnapshot()
	sourceBuffer := util.NewRingBuffer[*avframe.AVFrame](4)
	sourceReader := sourceBuffer.NewReaderAt(0)
	audioBuffer := util.NewRingBuffer[*avframe.AVFrame](4)
	audioReader := audioBuffer.NewReaderAt(0)

	call, remoteAudio, remoteVideo, audioPacketizer, videoPacketizer := newSIPDualReaderHarness(t, stream)
	started := make(chan sipMediaReader, 2)
	exitReached := make(chan sipMediaReader, 2)
	exitCallbackCompleted := make(chan sipMediaReader, 2)
	pumpCompleted := make(chan sipMediaReader, 2)
	releaseSourceExit := make(chan struct{})
	releaseTargetAudioExit := make(chan struct{})
	joinReached := make(chan struct{})
	releaseJoin := make(chan struct{})
	postJoinCompletions := make(chan uint32)
	releasePostJoin := make(chan struct{})
	var releaseSourceExitOnce sync.Once
	var releaseTargetAudioExitOnce sync.Once
	var releaseJoinOnce sync.Once
	var releasePostJoinOnce sync.Once
	t.Cleanup(func() {
		releaseSourceExitOnce.Do(func() { close(releaseSourceExit) })
		releaseTargetAudioExitOnce.Do(func() { close(releaseTargetAudioExit) })
		releaseJoinOnce.Do(func() { close(releaseJoin) })
		releasePostJoinOnce.Do(func() { close(releasePostJoin) })
	})
	const (
		sourcePumpCompleted uint32 = 1 << iota
		targetAudioPumpCompleted
		allPumpsCompleted = sourcePumpCompleted | targetAudioPumpCompleted
	)
	var completedPumps atomic.Uint32
	call.pumpObserver = &sipMediaPumpObserver{
		started: func(reader sipMediaReader) { started <- reader },
		exiting: func(reader sipMediaReader) {
			exitReached <- reader
			switch reader {
			case sipMediaReaderSource:
				<-releaseSourceExit
			case sipMediaReaderTargetAudio:
				<-releaseTargetAudioExit
			}
			exitCallbackCompleted <- reader
		},
		exited: func(reader sipMediaReader) {
			var completed uint32
			switch reader {
			case sipMediaReaderSource:
				completed = sourcePumpCompleted
			case sipMediaReaderTargetAudio:
				completed = targetAudioPumpCompleted
			}
			completedPumps.Or(completed)
			pumpCompleted <- reader
		},
		joining: func() {
			close(joinReached)
			<-releaseJoin
		},
		joined: func() {
			postJoinCompletions <- completedPumps.Load()
			<-releasePostJoin
		},
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		call.sendTranscodedAudioAndVideo(
			context.Background(), stream, snapshot, sourceReader, audioReader,
			audioPacketizer, lfertp.NewSession(0, 8000), call.conn, call.rtcpConn, call.remoteAddr,
			videoPacketizer, lfertp.NewSession(96, 90000), call.video,
		)
	}()

	startedReaders := make(map[sipMediaReader]bool, 2)
	for len(startedReaders) < 2 {
		reader := waitSIPReaderSignal(t, started, "media pump did not start")
		if startedReaders[reader] {
			t.Fatalf("media pump %s started more than once", reader)
		}
		startedReaders[reader] = true
	}
	sourceBuffer.Write(sipVideoFrame(avframe.FrameTypeKeyframe, 0xd0))
	assertRTPMarkers(t, remoteVideo, []byte{0xd0})
	audioBuffer.Write(sipTargetAudioFrame(0xd1))
	assertRTPMarkers(t, remoteAudio, []byte{0xd1})

	audioBuffer.Close()
	waitSIPSignal(t, joinReached, "media parent did not reach the pump join boundary")
	reachedReaders := make(map[sipMediaReader]bool, 2)
	for len(reachedReaders) < 2 {
		reader := waitSIPReaderSignal(t, exitReached, "media pump did not reach its exit callback")
		if reachedReaders[reader] {
			t.Fatalf("media pump %s reached its exit callback more than once", reader)
		}
		reachedReaders[reader] = true
	}
	sourceBuffer.Write(sipVideoFrame(avframe.FrameTypeInterframe, 0xee))
	releaseSourceExitOnce.Do(func() { close(releaseSourceExit) })
	releaseTargetAudioExitOnce.Do(func() { close(releaseTargetAudioExit) })
	callbackCompletedReaders := make(map[sipMediaReader]bool, 2)
	for len(callbackCompletedReaders) < 2 {
		reader := waitSIPReaderSignal(t, exitCallbackCompleted, "media pump exit callback did not complete")
		if callbackCompletedReaders[reader] {
			t.Fatalf("media pump %s completed its exit callback more than once", reader)
		}
		callbackCompletedReaders[reader] = true
	}
	releaseJoinOnce.Do(func() { close(releaseJoin) })
	var postJoin uint32
	select {
	case postJoin = <-postJoinCompletions:
	case <-time.After(2 * time.Second):
		t.Fatal("media parent did not continue after the pump join")
	}
	if postJoin != allPumpsCompleted {
		t.Fatalf("pump completions at post-join boundary = %02b, want %02b", postJoin, allPumpsCompleted)
	}
	completedReaders := make(map[sipMediaReader]bool, 2)
	for len(completedReaders) < 2 {
		reader := waitSIPReaderSignal(t, pumpCompleted, "media pump completion was not observed before post-join")
		if completedReaders[reader] {
			t.Fatalf("media pump %s completed more than once", reader)
		}
		completedReaders[reader] = true
	}
	releasePostJoinOnce.Do(func() { close(releasePostJoin) })
	waitSIPSendLoop(t, done)
	if got := call.snapshot().State; got != CallStateNetworkLost {
		t.Fatalf("terminal state = %s, want network_lost", got)
	}
	if got := call.rtpPacketsSent.Load(); got != 2 {
		t.Fatalf("RTP packets after terminal pump exit = %d, want two pre-terminal packets", got)
	}
	assertNoSIPRTPPacket(t, remoteAudio)
	assertNoSIPRTPPacket(t, remoteVideo)
}

func TestCallSessionOutboundActiveGenerationReplacementRejectsFrameAtFinalAdmission(t *testing.T) {
	stream := newSIPOverwriteStream(t, "sip/dual-reader-generation-replacement", 8, true)
	oldSnapshot := stream.StartupSnapshot()
	sourceReader := stream.RingBuffer().NewReaderAt(oldSnapshot.LiveCursor)
	defer sourceReader.Close()
	audioBuffer := util.NewRingBuffer[*avframe.AVFrame](4)
	audioReader := audioBuffer.NewReaderAt(0)
	call, remoteAudio, remoteVideo, audioPacketizer, videoPacketizer := newSIPDualReaderHarness(t, stream)
	call.stream = stream
	call.startupSnapshot = oldSnapshot
	call.transcodedAudio = audioReader
	var targetReleases atomic.Int32
	call.releaseAudio = func() { targetReleases.Add(1) }
	releaseSubscriber, err := stream.AddSubscriberForGeneration("sipgateway", oldSnapshot.Generation)
	if err != nil {
		t.Fatalf("AddSubscriberForGeneration: %v", err)
	}
	var subscriberReleases atomic.Int32
	call.releaseSubscriber = func() {
		subscriberReleases.Add(1)
		releaseSubscriber()
	}

	admissionEntered := make(chan struct{})
	admissionRelease := make(chan struct{})
	blockedAudioPacketizer := &sipAdmissionBlockingPacketizer{
		delegate: audioPacketizer,
		marker:   0xe2,
		entered:  admissionEntered,
		release:  admissionRelease,
	}
	ctx, cancel := bindSIPGeneration(context.Background(), oldSnapshot)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer call.releaseGenerationResources()
		call.sendTranscodedAudioAndVideo(
			ctx, stream, oldSnapshot, sourceReader, audioReader,
			blockedAudioPacketizer, lfertp.NewSession(0, 8000), call.conn, call.rtcpConn, call.remoteAddr,
			videoPacketizer, lfertp.NewSession(96, 90000), call.video,
		)
	}()

	stream.WriteFrame(sipVideoFrame(avframe.FrameTypeKeyframe, 0xd0))
	assertRTPMarkers(t, remoteVideo, []byte{0xd0})
	audioBuffer.Write(sipTargetAudioFrame(0xd1))
	assertRTPMarkers(t, remoteAudio, []byte{0xd1})
	audioBuffer.Write(sipTargetAudioFrame(0xe2))
	waitSIPSignal(t, admissionEntered, "frame did not reach the final send-admission boundary")

	stream.RemovePublisher()
	if err := stream.SetPublisher(&gatewayTestPublisher{id: "replacement", info: &avframe.MediaInfo{
		VideoCodec: avframe.CodecH264,
		AudioCodec: avframe.CodecG711A,
		SampleRate: 8000,
		Channels:   1,
	}}); err != nil {
		t.Fatalf("SetPublisher replacement: %v", err)
	}
	stream.WriteFrame(sipVideoFrame(avframe.FrameTypeKeyframe, 0xf1))
	audioBuffer.Write(sipTargetAudioFrame(0xf2))
	close(admissionRelease)
	waitSIPSendLoop(t, done)

	if got := call.rtpPacketsSent.Load(); got != 2 {
		t.Fatalf("RTP packets after generation replacement = %d, want only two pre-retirement packets", got)
	}
	assertNoSIPRTPPacket(t, remoteAudio)
	assertNoSIPRTPPacket(t, remoteVideo)
	call.Close()
	call.Close()
	if got := targetReleases.Load(); got != 1 {
		t.Fatalf("target-audio releases = %d, want 1", got)
	}
	if got := subscriberReleases.Load(); got != 1 {
		t.Fatalf("subscriber releases = %d, want 1", got)
	}
}

type sipAdmissionBlockingPacketizer struct {
	delegate lfertp.Packetizer
	marker   byte
	entered  chan struct{}
	release  <-chan struct{}
	once     sync.Once
}

func (p *sipAdmissionBlockingPacketizer) Packetize(frame *avframe.AVFrame, mtu int) ([]*pionrtp.Packet, error) {
	if frame != nil && len(frame.Payload) > 0 && frame.Payload[len(frame.Payload)-1] == p.marker {
		p.once.Do(func() { close(p.entered) })
		<-p.release
	}
	return p.delegate.Packetize(frame, mtu)
}

func newSIPDualReaderHarness(
	t *testing.T,
	stream *core.Stream,
) (*CallSession, *net.UDPConn, *net.UDPConn, lfertp.Packetizer, lfertp.Packetizer) {
	t.Helper()
	remoteAudioRTP, remoteAudioRTCP, err := listenLabUDPPair()
	if err != nil {
		t.Fatalf("listen remote audio pair: %v", err)
	}
	remoteVideoRTP, remoteVideoRTCP, err := listenLabUDPPair()
	if err != nil {
		t.Fatalf("listen remote video pair: %v", err)
	}
	localAudioRTP, localAudioRTCP, err := listenLabUDPPair()
	if err != nil {
		t.Fatalf("listen local audio pair: %v", err)
	}
	localVideoRTP, localVideoRTCP, err := listenLabUDPPair()
	if err != nil {
		t.Fatalf("listen local video pair: %v", err)
	}
	t.Cleanup(func() {
		_ = remoteAudioRTP.Close()
		_ = remoteAudioRTCP.Close()
		_ = remoteVideoRTP.Close()
		_ = remoteVideoRTCP.Close()
		_ = localAudioRTP.Close()
		_ = localAudioRTCP.Close()
		_ = localVideoRTP.Close()
		_ = localVideoRTCP.Close()
	})

	call := newCallSession(
		"dual-reader-"+stream.Key(), stream.Key(),
		negotiatedCodec{Codec: avframe.CodecG711U, PT: 0, ClockRate: 8000, EncodingName: "PCMU"},
		"outbound", localAudioRTP.LocalAddr().(*net.UDPAddr).Port, localAudioRTCP.LocalAddr().(*net.UDPAddr).Port,
	)
	call.configureMediaSockets(localAudioRTP, localAudioRTCP)
	call.remoteAddr = remoteAudioRTP.LocalAddr().(*net.UDPAddr)
	call.configureVideo(
		sipH264Codec,
		localVideoRTP.LocalAddr().(*net.UDPAddr).Port,
		localVideoRTCP.LocalAddr().(*net.UDPAddr).Port,
		"127.0.0.1",
		remoteVideoRTP.LocalAddr().(*net.UDPAddr).Port,
	)
	call.configureVideoSockets(localVideoRTP, localVideoRTCP)
	call.state = CallStateActive
	audioPacketizer, err := lfertp.NewPacketizer(avframe.CodecG711U)
	if err != nil {
		t.Fatalf("NewPacketizer G711U: %v", err)
	}
	videoPacketizer, err := lfertp.NewPacketizer(avframe.CodecH264)
	if err != nil {
		t.Fatalf("NewPacketizer H264: %v", err)
	}
	return call, remoteAudioRTP, remoteVideoRTP, audioPacketizer, videoPacketizer
}

func sipTargetAudioFrame(marker byte) *avframe.AVFrame {
	return avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecG711U, avframe.FrameTypeInterframe, 0, 0, []byte{marker})
}

func waitSIPSendLoop(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("SIP media pumps did not terminate within the deterministic bound")
	}
}

func waitSIPSignal(t *testing.T, signal <-chan struct{}, failure string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatal(failure)
	}
}

func waitSIPMarkerSignal(t *testing.T, markers <-chan byte, want byte) {
	t.Helper()
	select {
	case got := <-markers:
		if got != want {
			t.Fatalf("SIP media marker = %02x, want %02x", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for SIP media marker %02x", want)
	}
}

func waitSIPReaderSignal(t *testing.T, readers <-chan sipMediaReader, failure string) sipMediaReader {
	t.Helper()
	select {
	case reader := <-readers:
		return reader
	case <-time.After(2 * time.Second):
		t.Fatal(failure)
		return ""
	}
}

func assertNoSIPRTPPacket(t *testing.T, conn *net.UDPConn) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	buf := make([]byte, 2048)
	if _, _, err := conn.ReadFromUDP(buf); err == nil {
		t.Fatal("unexpected RTP packet after terminal generation replacement")
	} else {
		var netErr net.Error
		if !errors.As(err, &netErr) || !netErr.Timeout() {
			t.Fatalf("ReadFromUDP: %v", err)
		}
	}
}
