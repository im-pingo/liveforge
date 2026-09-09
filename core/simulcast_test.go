package core

import (
	"sync"
	"testing"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/pkg/avframe"
)

func testSimulcastFamily(t *testing.T, pause bool) (*Stream, *SimulcastFamily, *testPublisher) {
	t.Helper()
	s := NewStream("live/layers", newTestStreamConfig(), config.LimitsConfig{MaxSubscribersPerStream: 2}, nil)
	t.Cleanup(s.Close)
	p := &testPublisher{id: "family", info: &avframe.MediaInfo{VideoCodec: avframe.CodecVP8, AudioCodec: avframe.CodecOpus}}
	if err := s.SetPublisher(p); err != nil {
		t.Fatal(err)
	}
	f, err := s.AttachSimulcast(p, []config.LayerConfig{{RID: "h", MaxBitrate: 3}, {RID: "m", MaxBitrate: 2}, {RID: "l", MaxBitrate: 1}}, pause)
	if err != nil {
		t.Fatal(err)
	}
	return s, f, p
}

func TestSimulcastIsolationCanonicalAndSharedAudio(t *testing.T) {
	parent, family, _ := testSimulcastFamily(t, false)
	for i, rid := range []string{"h", "m", "l"} {
		if !family.WriteVideo(rid, avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecVP8, avframe.FrameTypeKeyframe, 0, 0, []byte{byte(i)})) {
			t.Fatal("video rejected")
		}
	}
	audio := avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecOpus, avframe.FrameTypeInterframe, 0, 0, []byte{7})
	if !family.WriteAudio(audio) {
		t.Fatal("audio rejected")
	}
	if audio.AudioCodecEpoch != 0 {
		t.Fatal("shared envelope mutated")
	}
	for i, rid := range []string{"h", "m", "l"} {
		layer, ok := family.Layer(rid)
		if !ok || (layer == parent) != (rid == "h") {
			t.Fatal("canonical mapping changed")
		}
		snap := layer.StartupSnapshot()
		if len(snap.ReplayFrames) != 2 || snap.ReplayFrames[0].Payload[0] != byte(i) || snap.ReplayFrames[1].AudioCodecEpoch == 0 {
			t.Fatalf("RID %s mixed media: %+v", rid, snap)
		}
		if rid != "h" && snap.PublisherID == parent.StartupSnapshot().PublisherID {
			t.Fatal("publisher identity shared")
		}
	}
}

func TestSimulcastFamilyAdmissionAndPauseResume(t *testing.T) {
	parent, family, _ := testSimulcastFamily(t, true)
	if active, _ := family.Processing("h"); !active {
		t.Fatal("canonical paused")
	}
	if active, _ := family.Processing("l"); active {
		t.Fatal("unused layer active")
	}
	low, rid, release, err := family.Acquire("low", "webrtc")
	if err != nil || rid != "l" {
		t.Fatalf("selection: %s %v", rid, err)
	}
	if active, _ := family.Processing("l"); !active {
		t.Fatal("lease did not wake layer before startup")
	}
	if parent.TotalSubscribers() != 1 || low.TotalSubscribers() != 1 {
		t.Fatal("family accounting missing")
	}
	if subscriberErr := parent.AddSubscriber("rtmp"); subscriberErr != nil {
		t.Fatal(subscriberErr)
	}
	if _, _, _, capacityErr := family.Acquire("m", "webrtc"); capacityErr == nil {
		t.Fatal("child bypassed family ceiling")
	}
	parent.RemoveSubscriber("rtmp")
	family.WriteVideo("l", avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecVP8, avframe.FrameTypeKeyframe, 0, 0, []byte{1}))
	_, oldEpoch := family.Processing("l")
	release()
	release()
	if active, _ := family.Processing("l"); active {
		t.Fatal("released layer still processing")
	}
	_, _, release, err = family.Acquire("l", "webrtc")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, epoch := family.Processing("l"); epoch <= oldEpoch {
		t.Fatal("resume did not invalidate depacketizer")
	}
	if len(low.StartupSnapshot().ReplayFrames) != 0 {
		t.Fatal("resumed startup retained stale GOP")
	}
}

func TestSimulcastReplacementClosesPrivateStreamsAndRejectsLateFrames(t *testing.T) {
	parent, family, pub := testSimulcastFamily(t, false)
	low, _ := family.Layer("l")
	done := low.StartupSnapshot().GenerationDone
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 100 {
			family.WriteVideo("l", avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecVP8, avframe.FrameTypeKeyframe, 0, 0, []byte{1}))
		}
	}()
	parent.RemovePublisherIf(pub)
	if err := parent.SetPublisher(&testPublisher{id: "replacement", info: pub.info}); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	select {
	case <-done:
	default:
		t.Fatal("child generation remains open")
	}
	if parent.Simulcast() != nil {
		t.Fatal("old family visible to new generation")
	}
	if family.WriteAudio(avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecOpus, avframe.FrameTypeInterframe, 0, 0, []byte{1})) {
		t.Fatal("late audio accepted")
	}
	if _, _, _, err := family.Acquire("auto", "webrtc"); err == nil {
		t.Fatal("old family admitted subscriber")
	}
}

func TestSimulcastPrivateLayersFollowHotPolicy(t *testing.T) {
	parent, family, _ := testSimulcastFamily(t, false)
	_, _, release, err := family.Acquire("low", "webrtc")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	policy := parent.Config()
	policy.GOPCacheMaxFrames = 1
	parent.UpdatePolicy(policy, config.LimitsConfig{MaxSubscribersPerStream: 3, MaxBitratePerStream: 500})
	_, _, second, err := family.Acquire("low", "webrtc")
	if err != nil {
		t.Fatal(err)
	}
	defer second()
	_, _, third, err := family.Acquire("low", "webrtc")
	if err != nil {
		t.Fatalf("updated parent limit did not reach child: %v", err)
	}
	defer third()
	low, _ := family.Layer("l")
	if low.Config().GOPCacheMaxFrames != 1 || low.Config().IdleTimeout != 0 || low.Config().NoPublisherTimeout != 0 {
		t.Fatal("child policy/timer ownership diverged")
	}
	if low.limits.MaxBitratePerStream != 500 {
		t.Fatal("child retained stale bitrate policy")
	}
}
