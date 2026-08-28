package gb28181

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtcp"
	pionrtp "github.com/pion/rtp/v2"
)

// RTPReceiver listens for RTP packets over UDP or TCP.
type RTPReceiver struct {
	conn        *net.UDPConn
	rtcpConn    *net.UDPConn
	publisher   *Publisher
	reorder     *reorderBuffer
	done        chan struct{}
	closed      bool
	mu          sync.Mutex
	closeOnce   sync.Once
	finishOnce  sync.Once
	runErr      error
	idleTimeout time.Duration
	rtcpRecv    atomic.Uint64
}

const gbRTPIdleTimeout = 30 * time.Second

var newRTPReceiver = NewRTPReceiver

// NewRTPReceiver creates a new RTP receiver bound to a UDP port.
func NewRTPReceiver(port int, publisher *Publisher) (*RTPReceiver, error) {
	addr := &net.UDPAddr{Port: port}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen UDP :%d: %w", port, err)
	}
	rtcpPort := port + 1
	if port == 0 {
		rtcpPort = conn.LocalAddr().(*net.UDPAddr).Port + 1
	}
	rtcpConn, err := net.ListenUDP("udp", &net.UDPAddr{Port: rtcpPort})
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("listen RTCP UDP :%d: %w", rtcpPort, err)
	}

	return &RTPReceiver{
		conn:        conn,
		rtcpConn:    rtcpConn,
		publisher:   publisher,
		reorder:     newReorderBuffer(50),
		done:        make(chan struct{}),
		idleTimeout: gbRTPIdleTimeout,
	}, nil
}

// Run starts the receive loop. Blocks until closed or error.
func (r *RTPReceiver) Run() {
	results := make(chan error, 2)
	go func() { results <- r.runRTP() }()
	go func() { results <- r.runRTCP() }()
	first := <-results
	r.closeSockets()
	second := <-results
	if first == nil {
		first = second
	}
	r.finish(first)
}

func (r *RTPReceiver) runRTP() error {
	buf := make([]byte, 2048)
	for {
		idleTimeout := r.idleTimeout
		if idleTimeout <= 0 {
			idleTimeout = gbRTPIdleTimeout
		}
		if err := r.conn.SetReadDeadline(time.Now().Add(idleTimeout)); err != nil {
			return r.readError("RTP", err)
		}
		n, _, err := r.conn.ReadFromUDP(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				return fmt.Errorf("GB28181 RTP media idle timeout after %s", idleTimeout)
			}
			return r.readError("RTP", err)
		}

		if n < 12 { // minimum RTP header
			continue
		}

		var pkt pionrtp.Packet
		if err := pkt.Unmarshal(buf[:n]); err != nil {
			continue
		}

		// Feed through reorder buffer
		r.reorder.push(&pkt, func(p *pionrtp.Packet) {
			r.publisher.FeedRTP(p)
		})
	}
}

func (r *RTPReceiver) runRTCP() error {
	buf := make([]byte, 2048)
	for {
		if err := r.rtcpConn.SetReadDeadline(time.Now().Add(gbRTPIdleTimeout)); err != nil {
			return r.readError("RTCP", err)
		}
		n, _, err := r.rtcpConn.ReadFromUDP(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			return r.readError("RTCP", err)
		}
		packets, err := rtcp.Unmarshal(buf[:n])
		if err == nil {
			r.rtcpRecv.Add(uint64(len(packets)))
		}
	}
}

func (r *RTPReceiver) readError(track string, err error) error {
	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	if closed || errors.Is(err, net.ErrClosed) && r.isStopping() {
		return nil
	}
	slog.Warn("media receiver error", "module", "gb28181", "track", track, "error", err)
	return fmt.Errorf("GB28181 %s receiver: %w", track, err)
}

func (r *RTPReceiver) isStopping() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed
}

func (r *RTPReceiver) closeSockets() {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	r.closeOnce.Do(func() {
		_ = r.conn.Close()
		_ = r.rtcpConn.Close()
	})
}

func (r *RTPReceiver) finish(err error) {
	r.finishOnce.Do(func() {
		r.mu.Lock()
		r.runErr = err
		r.mu.Unlock()
		close(r.done)
	})
}

// Close stops the receiver.
func (r *RTPReceiver) Close() {
	r.closeSockets()
}

// Done closes when both RTP and RTCP workers have terminated.
func (r *RTPReceiver) Done() <-chan struct{} { return r.done }

// Err returns the terminal receive error after Done closes.
func (r *RTPReceiver) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runErr
}

// LocalPort returns the local UDP port.
func (r *RTPReceiver) LocalPort() int {
	return r.conn.LocalAddr().(*net.UDPAddr).Port
}

// RTCPPacketsReceived returns the number of valid control packets parsed by LiveForge.
func (r *RTPReceiver) RTCPPacketsReceived() uint64 { return r.rtcpRecv.Load() }

// reorderBuffer reorders RTP packets by sequence number for UDP delivery.
type reorderBuffer struct {
	buf      map[uint16]*pionrtp.Packet
	nextSeq  uint16
	maxDelay uint16
	started  bool
}

func newReorderBuffer(maxDelay uint16) *reorderBuffer {
	return &reorderBuffer{
		buf:      make(map[uint16]*pionrtp.Packet),
		maxDelay: maxDelay,
	}
}

func (b *reorderBuffer) push(pkt *pionrtp.Packet, emit func(*pionrtp.Packet)) {
	seq := pkt.SequenceNumber

	if !b.started {
		b.started = true
		b.nextSeq = seq
	}

	b.buf[seq] = pkt

	// Flush consecutive packets starting from nextSeq
	for {
		p, ok := b.buf[b.nextSeq]
		if !ok {
			break
		}
		delete(b.buf, b.nextSeq)
		emit(p)
		b.nextSeq++
	}

	// Skip gaps exceeding maxDelay
	if uint16(len(b.buf)) > b.maxDelay {
		// Find the lowest sequence in the buffer and flush up to it
		minSeq := seq
		for s := range b.buf {
			if seqDiff(s, b.nextSeq) < seqDiff(minSeq, b.nextSeq) {
				minSeq = s
			}
		}
		// Skip to minSeq
		b.nextSeq = minSeq
		for {
			p, ok := b.buf[b.nextSeq]
			if !ok {
				break
			}
			delete(b.buf, b.nextSeq)
			emit(p)
			b.nextSeq++
		}
	}
}

// seqDiff computes the forward distance from a to b in uint16 space.
func seqDiff(a, b uint16) uint16 {
	return a - b
}

// ReadTCPRTPPacket reads a single RTP packet from a TCP stream (RFC 4571: 2-byte length prefix).
func ReadTCPRTPPacket(conn net.Conn) (*pionrtp.Packet, error) {
	lenBuf := make([]byte, 2)
	if _, err := fullRead(conn, lenBuf); err != nil {
		return nil, err
	}
	pktLen := int(binary.BigEndian.Uint16(lenBuf))
	if pktLen < 12 || pktLen > 65535 {
		return nil, fmt.Errorf("invalid RTP packet length: %d", pktLen)
	}

	data := make([]byte, pktLen)
	if _, err := fullRead(conn, data); err != nil {
		return nil, err
	}

	var pkt pionrtp.Packet
	if err := pkt.Unmarshal(data); err != nil {
		return nil, fmt.Errorf("unmarshal RTP: %w", err)
	}
	return &pkt, nil
}

func fullRead(conn net.Conn, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := conn.Read(buf[total:])
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}
