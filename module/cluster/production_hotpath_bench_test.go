package cluster

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"testing"

	"github.com/im-pingo/liveforge/module/rtmp"
	"github.com/im-pingo/liveforge/pkg/avframe"
	flvpkg "github.com/im-pingo/liveforge/pkg/muxer/flv"
	pkgrtp "github.com/im-pingo/liveforge/pkg/rtp"
	pionrtp "github.com/pion/rtp/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func BenchmarkRTMPRelaySendMediaFrameProduction(b *testing.B) {
	benchmarks := []struct {
		name  string
		frame *avframe.AVFrame
	}{
		{name: "H264Video1200B", frame: benchmarkH264Frame(1200)},
		{name: "AACAudio160B", frame: benchmarkAACFrame(160)},
	}
	for _, benchmark := range benchmarks {
		b.Run(benchmark.name, func(b *testing.B) {
			wantPayload := append([]byte(nil), benchmark.frame.Payload...)
			strictSink := &rtmpValidationSink{frame: benchmark.frame}
			strictConn := newBenchmarkRTMPConn(strictSink)
			for range 3 {
				if err := strictConn.sendMediaFrame(benchmark.frame); err != nil {
					b.Fatalf("RTMP production preflight: %v", err)
				}
			}
			if err := strictSink.validate(3); err != nil {
				b.Fatal(err)
			}

			sink := &countingBenchmarkSink{}
			conn := newBenchmarkRTMPConn(sink)
			metrics := newRelayMetrics()
			ctx := observeRelay(context.Background(), metrics, relayDirectionForward, "rtmp")

			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if err := conn.sendMediaFrame(benchmark.frame); err != nil {
					b.Fatal(err)
				}
				recordRelayBytes(ctx, int64(len(benchmark.frame.Payload)))
			}
			b.StopTimer()
			flushRelayBytes(ctx)

			if sink.writes != uint64(b.N) || sink.wireBytes <= uint64(b.N*len(benchmark.frame.Payload)) || sink.checksum == 0 { // #nosec G115 -- benchmark counters are bounded by testing.B.
				b.Fatalf("RTMP sink writes=%d bytes=%d checksum=%d, want %d framed writes", sink.writes, sink.wireBytes, sink.checksum, b.N)
			}
			if got := testutil.ToFloat64(metrics.bytesTotal.WithLabelValues(relayDirectionForward, "rtmp")); got != float64(b.N*len(benchmark.frame.Payload)) {
				b.Fatalf("RTMP relay byte accounting = %.0f, want %d payload bytes", got, b.N*len(benchmark.frame.Payload))
			}
			if !bytes.Equal(benchmark.frame.Payload, wantPayload) {
				b.Fatal("RTMP send mutated the fixture payload")
			}
		})
	}
}

func BenchmarkRTSPRelaySendFrameProduction(b *testing.B) {
	benchmarks := []struct {
		name           string
		size           int
		wantPackets    uint64
		wantFragmented bool
	}{
		{name: "H264Video1200B_SingleNAL", size: 1200, wantPackets: 1},
		{name: "H264Video3000B_FUA", size: 3000, wantPackets: 3, wantFragmented: true},
	}
	for _, benchmark := range benchmarks {
		b.Run(benchmark.name, func(b *testing.B) {
			transport := NewRTSPTransport(defaultClusterRTSPConfig())
			frame := benchmarkH264Frame(benchmark.size)
			wantPayload := append([]byte(nil), frame.Payload...)

			strictPacketizer, err := pkgrtp.NewPacketizer(avframe.CodecH264)
			if err != nil {
				b.Fatal(err)
			}
			strictSink := &rtspValidationSink{}
				if sendErr := transport.sendRTSPFrame(context.Background(), strictSink, frame, strictPacketizer, pkgrtp.NewSession(96, 90000), 0); sendErr != nil {
					b.Fatalf("RTSP production preflight: %v", sendErr)
				}
				if validateErr := strictSink.validate(benchmark.wantPackets, benchmark.wantFragmented); validateErr != nil {
					b.Fatal(validateErr)
			}

			packetizer, err := pkgrtp.NewPacketizer(avframe.CodecH264)
			if err != nil {
				b.Fatal(err)
			}
			session := pkgrtp.NewSession(96, 90000)
			sink := &countingBenchmarkSink{}
			metrics := newRelayMetrics()
			ctx := observeRelay(context.Background(), metrics, relayDirectionForward, "rtsp")

			// A bounded io.Writer exercises net.Buffers' sequential fallback. Real
			// TCP writev and network syscall costs are intentionally out of scope.
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if err := transport.sendRTSPFrame(ctx, sink, frame, packetizer, session, 0); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			flushRelayBytes(ctx)

			wantWrites := uint64(b.N) * benchmark.wantPackets * 2                                                          // #nosec G115 -- benchmark counters are bounded by testing.B.
			if sink.writes != wantWrites || sink.wireBytes <= uint64(b.N)*benchmark.wantPackets*12 || sink.checksum == 0 { // #nosec G115 -- benchmark counters are bounded by testing.B.
				b.Fatalf("RTSP sink writes=%d bytes=%d checksum=%d, want %d bounded-writer calls", sink.writes, sink.wireBytes, sink.checksum, wantWrites)
			}
			if got := testutil.ToFloat64(metrics.bytesTotal.WithLabelValues(relayDirectionForward, "rtsp")); got != float64(sink.wireBytes) {
				b.Fatalf("RTSP relay byte accounting = %.0f, want %d framed bytes", got, sink.wireBytes)
			}
			if !bytes.Equal(frame.Payload, wantPayload) {
				b.Fatal("RTSP packetization mutated the fixture payload")
			}
		})
	}
}

const relayBenchmarkBatchSize = 1024

func BenchmarkRelayObservationAccounting(b *testing.B) {
	const bytesPerRecord int64 = 1024

	b.Run("FirstObservation", func(b *testing.B) {
		counter := newRelayMetrics().bytesCounter(relayDirectionForward, "benchmark-first")
		observations := newRelayBenchmarkObservations(counter)
		b.ReportAllocs()
		b.ResetTimer()
		b.StopTimer()
		for processed := 0; processed < b.N; {
			count := min(relayBenchmarkBatchSize, b.N-processed)
			for index := range count {
				observations[index].firstFlush.Store(false)
				observations[index].pendingBytes.Store(0)
			}
			b.StartTimer()
			for index := range count {
				observations[index].recordBytes(bytesPerRecord)
			}
			b.StopTimer()
			for index := range count {
				if !observations[index].firstFlush.Load() || observations[index].pendingBytes.Load() != 0 {
					b.Fatal("first-observation fixture did not take the immediate flush path")
				}
			}
			processed += count
		}
		if got := testutil.ToFloat64(counter); got != float64(int64(b.N)*bytesPerRecord) {
			b.Fatalf("first-observation bytes = %.0f, want %d", got, int64(b.N)*bytesPerRecord)
		}
	})

	b.Run("BelowThresholdBatch", func(b *testing.B) {
		counter := newRelayMetrics().bytesCounter(relayDirectionForward, "benchmark-batch")
		observations := newRelayBenchmarkObservations(counter)
		b.ReportAllocs()
		b.ResetTimer()
		b.StopTimer()
		for processed := 0; processed < b.N; {
			count := min(relayBenchmarkBatchSize, b.N-processed)
			for index := range count {
				observations[index].firstFlush.Store(true)
				observations[index].pendingBytes.Store(0)
			}
			b.StartTimer()
			for index := range count {
				observations[index].recordBytes(bytesPerRecord)
			}
			b.StopTimer()
			for index := range count {
				if observations[index].pendingBytes.Load() != bytesPerRecord {
					b.Fatal("below-threshold fixture did not retain the pending bytes")
				}
			}
			processed += count
		}
		if got := testutil.ToFloat64(counter); got != 0 {
			b.Fatalf("below-threshold metric = %.0f, want 0", got)
		}
	})

	b.Run("ThresholdFlush", func(b *testing.B) {
		counter := newRelayMetrics().bytesCounter(relayDirectionForward, "benchmark-threshold")
		observations := newRelayBenchmarkObservations(counter)
		b.ReportAllocs()
		b.ResetTimer()
		b.StopTimer()
		for processed := 0; processed < b.N; {
			count := min(relayBenchmarkBatchSize, b.N-processed)
			for index := range count {
				observations[index].firstFlush.Store(true)
				observations[index].pendingBytes.Store(relayMetricsFlushBytes - bytesPerRecord)
			}
			b.StartTimer()
			for index := range count {
				observations[index].recordBytes(bytesPerRecord)
			}
			b.StopTimer()
			for index := range count {
				if observations[index].pendingBytes.Load() != 0 {
					b.Fatal("threshold fixture did not flush pending bytes")
				}
			}
			processed += count
		}
		if got := testutil.ToFloat64(counter); got != float64(int64(b.N)*relayMetricsFlushBytes) {
			b.Fatalf("threshold-flush bytes = %.0f, want %d", got, int64(b.N)*relayMetricsFlushBytes)
		}
	})

	b.Run("TerminalFlush", func(b *testing.B) {
		counter := newRelayMetrics().bytesCounter(relayDirectionForward, "benchmark-terminal")
		observations := newRelayBenchmarkObservations(counter)
		b.ReportAllocs()
		b.ResetTimer()
		b.StopTimer()
		for processed := 0; processed < b.N; {
			count := min(relayBenchmarkBatchSize, b.N-processed)
			for index := range count {
				observations[index].firstFlush.Store(true)
				observations[index].pendingBytes.Store(bytesPerRecord)
			}
			b.StartTimer()
			for index := range count {
				observations[index].flushBytes()
			}
			b.StopTimer()
			for index := range count {
				if observations[index].pendingBytes.Load() != 0 {
					b.Fatal("terminal fixture did not flush pending bytes")
				}
			}
			processed += count
		}
		if got := testutil.ToFloat64(counter); got != float64(int64(b.N)*bytesPerRecord) {
			b.Fatalf("terminal-flush bytes = %.0f, want %d", got, int64(b.N)*bytesPerRecord)
		}
	})
}

func newBenchmarkRTMPConn(writer io.Writer) *rtmpConn {
	// sendMediaFrame uses only cw, muxer, and flvBuf. conn and cr belong to
	// handshake/read paths and are intentionally absent from this send fixture.
	return &rtmpConn{
		cw:     rtmp.NewChunkWriter(writer, rtmp.DefaultChunkSize),
		flvBuf: bytes.Buffer{},
		muxer:  flvpkg.NewMuxer(),
	}
}

type countingBenchmarkSink struct {
	writes    uint64
	wireBytes uint64
	checksum  uint64
}

func (s *countingBenchmarkSink) Write(data []byte) (int, error) {
	s.writes++
	s.wireBytes += uint64(len(data))
	if len(data) > 0 {
		s.checksum += uint64(len(data)) + uint64(data[0]) + uint64(data[len(data)-1])
	}
	return len(data), nil
}

type rtmpValidationSink struct {
	frame              *avframe.AVFrame
	writes             uint64
	wireBytes          uint64
	continuationChunks uint64
	body               []byte
}

func (s *rtmpValidationSink) Write(data []byte) (int, error) {
	if len(data) < 12 || data[0] != 6 {
		return 0, fmt.Errorf("invalid RTMP fmt-0 chunk framing")
	}
		wantType := rtmp.MsgAudio
	if s.frame.MediaType.IsVideo() {
		wantType = rtmp.MsgVideo
	}
	if data[7] != wantType {
		return 0, fmt.Errorf("RTMP message type=%d, want %d", data[7], wantType)
	}
	messageLength := int(data[4])<<16 | int(data[5])<<8 | int(data[6])
	body := make([]byte, 0, messageLength)
	for offset, remaining := 12, messageLength; remaining > 0; {
		chunkLength := min(remaining, rtmp.DefaultChunkSize)
		if offset+chunkLength > len(data) {
			return 0, fmt.Errorf("truncated RTMP message body")
		}
		body = append(body, data[offset:offset+chunkLength]...)
		offset += chunkLength
		remaining -= chunkLength
		if remaining > 0 {
			if offset >= len(data) || data[offset] != byte(3<<6|6) {
				return 0, fmt.Errorf("invalid RTMP continuation header at offset %d", offset)
			}
			s.continuationChunks++
			offset++
		}
		if remaining == 0 && offset != len(data) {
			return 0, fmt.Errorf("unexpected RTMP trailing bytes: %d", len(data)-offset)
		}
	}
	s.writes++
	s.wireBytes += uint64(len(data))
	s.body = body
	return len(data), nil
}

func (s *rtmpValidationSink) validate(wantWrites uint64) error {
	if s.writes != wantWrites || s.wireBytes == 0 || s.continuationChunks < wantWrites {
		return fmt.Errorf("RTMP preflight writes=%d bytes=%d continuations=%d", s.writes, s.wireBytes, s.continuationChunks)
	}
	if s.frame.MediaType.IsVideo() {
		if len(s.body) != len(s.frame.Payload)+5 || s.body[0] != 0x17 || s.body[1] != 1 ||
			s.body[2] != 0 || s.body[3] != 0 || s.body[4] != 0 || !bytes.Equal(s.body[5:], s.frame.Payload) {
			return fmt.Errorf("invalid RTMP H.264 FLV body")
		}
		return nil
	}
	if len(s.body) != len(s.frame.Payload)+2 || s.body[0] != 0xaf || s.body[1] != 1 || !bytes.Equal(s.body[2:], s.frame.Payload) {
		return fmt.Errorf("invalid RTMP AAC FLV body")
	}
	return nil
}

type rtspValidationSink struct {
	pendingPayload int
	packets        uint64
	wireBytes      uint64
	markerPackets  uint64
	lastMarker     bool
	haveSequence   bool
	lastSequence   uint16
	haveSSRC       bool
	ssrc           uint32
	fragmented     bool
	fragmentStart  bool
	fragmentEnd    bool
}

func (s *rtspValidationSink) Write(data []byte) (int, error) {
	if s.pendingPayload == 0 {
		if len(data) != 4 || data[0] != '$' || data[1] != 0 {
			return 0, fmt.Errorf("invalid RTSP interleaved header")
		}
		s.pendingPayload = int(binary.BigEndian.Uint16(data[2:4]))
		s.wireBytes += uint64(len(data))
		return len(data), nil
	}
	if len(data) != s.pendingPayload {
		return 0, fmt.Errorf("invalid RTP payload length: got %d, want %d", len(data), s.pendingPayload)
	}
	var packet pionrtp.Packet
	if err := packet.Unmarshal(data); err != nil {
		return 0, fmt.Errorf("unmarshal RTP packet: %w", err)
	}
	if packet.Version != 2 || packet.PayloadType != 96 || packet.Timestamp != 90000 || len(packet.Payload) == 0 {
		return 0, fmt.Errorf("invalid RTP header version=%d pt=%d timestamp=%d payload=%d", packet.Version, packet.PayloadType, packet.Timestamp, len(packet.Payload))
	}
	if s.haveSequence && packet.SequenceNumber != s.lastSequence+1 {
		return 0, fmt.Errorf("non-monotonic RTP sequence: got %d after %d", packet.SequenceNumber, s.lastSequence)
	}
	if s.haveSSRC && packet.SSRC != s.ssrc {
		return 0, fmt.Errorf("RTP SSRC changed from %d to %d", s.ssrc, packet.SSRC)
	}
	s.haveSequence = true
	s.lastSequence = packet.SequenceNumber
	s.haveSSRC = true
	s.ssrc = packet.SSRC
	if packet.Marker {
		s.markerPackets++
	}
	s.lastMarker = packet.Marker
	if packet.Payload[0]&0x1f == 28 {
		s.fragmented = true
		if len(packet.Payload) < 2 {
			return 0, fmt.Errorf("truncated H.264 FU-A payload")
		}
		s.fragmentStart = s.fragmentStart || packet.Payload[1]&0x80 != 0
		s.fragmentEnd = s.fragmentEnd || packet.Payload[1]&0x40 != 0
	} else if packet.Payload[0]&0x1f != 5 {
		return 0, fmt.Errorf("unexpected H.264 NAL type %d", packet.Payload[0]&0x1f)
	}
	s.pendingPayload = 0
	s.packets++
	s.wireBytes += uint64(len(data))
	return len(data), nil
}

func (s *rtspValidationSink) validate(wantPackets uint64, wantFragmented bool) error {
	if s.pendingPayload != 0 || s.packets != wantPackets || s.markerPackets != 1 || !s.lastMarker || s.wireBytes == 0 {
		return fmt.Errorf("RTSP preflight packets=%d marker=%d last_marker=%v bytes=%d pending=%d, want %d packets", s.packets, s.markerPackets, s.lastMarker, s.wireBytes, s.pendingPayload, wantPackets)
	}
	if s.fragmented != wantFragmented {
		return fmt.Errorf("RTSP fragmented=%v, want %v", s.fragmented, wantFragmented)
	}
	if wantFragmented && (!s.fragmentStart || !s.fragmentEnd) {
		return fmt.Errorf("RTSP FU-A missing start/end fragments")
	}
	return nil
}

func newRelayBenchmarkObservations(counter prometheus.Counter) []relayObservation {
	observations := make([]relayObservation, relayBenchmarkBatchSize)
	for index := range observations {
		observations[index].bytesTotal = counter
	}
	return observations
}

func benchmarkH264Frame(size int) *avframe.AVFrame {
	payload := make([]byte, size)
	binary.BigEndian.PutUint32(payload[:4], uint32(size-4)) // #nosec G115 -- benchmark fixture sizes are small positive constants.
	payload[4] = 0x65
	for index := 5; index < len(payload); index++ {
		payload[index] = byte(index)
	}
	return avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 1000, 1000, payload)
}

func benchmarkAACFrame(size int) *avframe.AVFrame {
	payload := make([]byte, size)
	for index := range payload {
		payload[index] = byte(index)
	}
	return avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe, 1000, 1000, payload)
}
