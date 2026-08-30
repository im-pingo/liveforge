package webrtc

import (
	"sync"
	"sync/atomic"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
)

type rtpTransportStats struct {
	packets atomic.Uint64
	bytes   atomic.Uint64
}

type rtpStatsInterceptorFactory struct {
	created chan *rtpPeerStats
}

func newRTPStatsInterceptorFactory() *rtpStatsInterceptorFactory {
	return &rtpStatsInterceptorFactory{created: make(chan *rtpPeerStats, 1)}
}

func (f *rtpStatsInterceptorFactory) NewInterceptor(string) (interceptor.Interceptor, error) {
	peer := &rtpPeerStats{}
	f.created <- peer
	return &rtpStatsInterceptor{peer: peer}, nil
}

type rtpPeerStats struct {
	counters sync.Map // SSRC -> *rtpTransportStats
}

func (s *rtpPeerStats) snapshot(ssrc uint32) (packets, bytes uint64) {
	value, ok := s.counters.Load(ssrc)
	if !ok {
		return 0, 0
	}
	counter := value.(*rtpTransportStats)
	return counter.packets.Load(), counter.bytes.Load()
}

type rtpStatsInterceptor struct {
	interceptor.NoOp
	peer *rtpPeerStats
}

func (i *rtpStatsInterceptor) BindLocalStream(info *interceptor.StreamInfo, writer interceptor.RTPWriter) interceptor.RTPWriter {
	counter := &rtpTransportStats{}
	i.peer.counters.Store(info.SSRC, counter)
	return interceptor.RTPWriterFunc(func(header *rtp.Header, payload []byte, attributes interceptor.Attributes) (int, error) {
		n, err := writer.Write(header, payload, attributes)
		if err == nil {
			counter.packets.Add(1)
			if size, ok := rtpPacketWireSize(header, payload); ok {
				counter.bytes.Add(size)
			}
		}
		return n, err
	})
}

func rtpPacketWireSize(header *rtp.Header, payload []byte) (uint64, bool) {
	if header == nil {
		return 0, false
	}
	headerSize, ok := checkedUint64(header.MarshalSize())
	if !ok {
		return 0, false
	}
	payloadSize, ok := checkedUint64(len(payload))
	if !ok || ^uint64(0)-headerSize < payloadSize {
		return 0, false
	}
	return headerSize + payloadSize, true
}

func checkedUint64(value int) (uint64, bool) {
	if value < 0 {
		return 0, false
	}
	return uint64(value), true
}

func (i *rtpStatsInterceptor) UnbindLocalStream(info *interceptor.StreamInfo) {
	i.peer.counters.Delete(info.SSRC)
}
