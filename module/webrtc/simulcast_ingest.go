package webrtc

import (
	"sync"
	"time"

	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	pkgrtp "github.com/im-pingo/liveforge/pkg/rtp"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
)

type whipSimulcastIngest struct {
	mu     sync.Mutex
	wg     sync.WaitGroup
	closed bool
	seen   map[string]bool
	stream *core.Stream
	pub    *WHIPPublisher
	sess   *Session
	plan   *whipSimulcastPlan
	family *core.SimulcastFamily
	ready  chan struct{}
	clock  *whipMediaClock
	ctx    core.EventContext
}

func newWHIPSimulcastIngest(stream *core.Stream, pub *WHIPPublisher, sess *Session, plan *whipSimulcastPlan, ctx *core.EventContext) *whipSimulcastIngest {
	return &whipSimulcastIngest{stream: stream, pub: pub, sess: sess, plan: plan, ctx: *ctx, seen: make(map[string]bool), ready: make(chan struct{}), clock: newWHIPMediaClock()}
}

func (s *whipSimulcastIngest) onTrack(track *webrtc.TrackRemote) {
	codec := mimeToCodecType(track.Codec().MimeType)
	key := track.RID()
	if codec.IsAudio() {
		key = "@audio"
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	valid := codec.IsAudio() && track.RID() == ""
	if codec.IsVideo() {
		for _, layer := range s.plan.layers {
			valid = valid || layer.RID == key
		}
	}
	if !valid || s.seen[key] {
		s.mu.Unlock()
		go s.sess.Close()
		return
	}
	s.seen[key] = true
	info := *s.pub.info.Load()
	if codec.IsVideo() {
		if info.VideoCodec != 0 && info.VideoCodec != codec {
			s.mu.Unlock()
			go s.sess.Close()
			return
		}
		info.VideoCodec = codec
	} else {
		info.AudioCodec, info.SampleRate, info.Channels = codec, int(track.Codec().ClockRate), int(track.Codec().Channels)
	}
	s.pub.info.Store(&info)
	if s.family == nil && info.VideoCodec != 0 && (!s.plan.expectsAudio || info.AudioCodec != 0) {
		if err := s.stream.SetPublisher(s.pub); err != nil {
			s.mu.Unlock()
			go s.sess.Close()
			return
		}
		family, err := s.stream.AttachSimulcast(s.pub, s.plan.layers, s.plan.pause)
		if err != nil {
			s.mu.Unlock()
			go s.sess.Close()
			return
		}
		s.family = family
		startup := s.stream.StartupSnapshot()
		s.ctx.PublisherID, s.ctx.StreamInstanceID, s.ctx.PublisherGeneration = s.pub.ID(), startup.StreamInstanceID, startup.Generation
		if !s.sess.startLifecycle(s.sess.module.server.GetEventBus(), core.EventPublish, &s.ctx) {
			s.mu.Unlock()
			go s.sess.Close()
			return
		}
		close(s.ready)
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			select {
			case <-s.sess.done:
			case <-startup.GenerationDone:
				go s.sess.Close()
			}
		}()
	}
	s.wg.Add(1)
	if codec.IsVideo() {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			requestWHIPKeyframes(s.pub.pc, uint32(track.SSRC()), s.sess.done, 2*time.Second)
		}()
	}
	s.mu.Unlock()
	defer s.wg.Done()
	select {
	case <-s.sess.done:
		return
	case <-s.ready:
	}
	s.mu.Lock()
	family := s.family
	s.mu.Unlock()
	if !family.Active() {
		return
	}
	dp, err := pkgrtp.NewDepacketizer(codec)
	if err != nil {
		go s.sess.Close()
		return
	}
	options := &whipTrackReadOptions{active: family.Active}
	if codec.IsVideo() {
		options.processing = func() (bool, uint64) { return family.Processing(key) }
		options.write = func(frame *avframe.AVFrame, epoch uint64) bool { return family.WriteVideoEpoch(key, epoch, frame) }
		options.resume = func() {
			_ = s.pub.pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(track.SSRC())}})
		}
	} else {
		options.write = func(frame *avframe.AVFrame, _ uint64) bool { return family.WriteAudio(frame) }
	}
	readTrackLoopWithOptions(track, dp, s.stream, s.pub, codec, s.clock, options)
}

func (s *whipSimulcastIngest) close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	// PC closure unblocks every TrackRemote.Read before joining the ingest and
	// feedback loops. The admission mutex prevents late OnTrack WG additions.
	_ = s.pub.Close()
	s.wg.Wait()
	s.stream.RemovePublisherIf(s.pub)
	s.sess.stopLifecycle(s.sess.module.server.GetEventBus(), core.EventPublishStop, &s.ctx)
}
