package gb28181

import (
	"encoding/binary"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	pionrtp "github.com/pion/rtp/v2"
)

// RTPReceiver listens for RTP packets over UDP or TCP.
type RTPReceiver struct {
	conn      *net.UDPConn
	publisher *Publisher
	reorder   *reorderBuffer
	done      chan struct{}
	closed    bool
	mu        sync.Mutex
}

// NewRTPReceiver creates a new RTP receiver bound to a UDP port.
func NewRTPReceiver(port int, publisher *Publisher) (*RTPReceiver, error) {
	addr := &net.UDPAddr{Port: port}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen UDP :%d: %w", port, err)
	}

	return &RTPReceiver{
		conn:      conn,
		publisher: publisher,
		reorder:   newReorderBuffer(50),
		done:      make(chan struct{}),
	}, nil
}

// Run starts the receive loop. Blocks until closed or error.
func (r *RTPReceiver) Run() {
	buf := make([]byte, 2048)
	for {
		r.conn.SetReadDeadline(time.Now().Add(30 * time.Second))
		n, _, err := r.conn.ReadFromUDP(buf)
		if err != nil {
			r.mu.Lock()
			closed := r.closed
			r.mu.Unlock()
			if closed {
				return
			}
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				slog.Debug("rtp receiver timeout", "module", "gb28181")
				continue
			}
			slog.Warn("rtp receiver error", "module", "gb28181", "error", err)
			return
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

// Close stops the receiver.
func (r *RTPReceiver) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.closed = true
	close(r.done)
	r.conn.Close()
}

// LocalPort returns the local UDP port.
func (r *RTPReceiver) LocalPort() int {
	return r.conn.LocalAddr().(*net.UDPAddr).Port
}

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
