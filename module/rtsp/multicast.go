package rtsp

import (
	"fmt"
	"net"
	"sync"
	"sync/atomic"

	"github.com/im-pingo/liveforge/config"
	"golang.org/x/net/ipv4"
)

// MulticastTransport sends RTP/RTCP to a multicast group address.
// Multiple subscribers share the same multicast stream; each subscriber
// joins the group on their end and receives via IGMP.
type MulticastTransport struct {
	rtpConn  net.PacketConn
	rtcpConn net.PacketConn
	rtpAddr  *net.UDPAddr
	rtcpAddr *net.UDPAddr
	rtpPort  int
	rtcpPort int
	done     chan struct{}
	closed   bool
	mu       sync.Mutex
}

// multicastPortCounter provides monotonically increasing port numbers
// for multicast streams, starting from the configured base port.
var multicastPortCounter atomic.Int64

// InitMulticastPorts sets the starting port for multicast allocation.
func InitMulticastPorts(basePort int) {
	multicastPortCounter.Store(int64(basePort))
}

// NewMulticastTransport creates a transport that sends to a multicast group.
func NewMulticastTransport(cfg config.MulticastConfig) (*MulticastTransport, error) {
	groupIP := net.ParseIP(cfg.Address)
	if groupIP == nil {
		return nil, fmt.Errorf("invalid multicast address: %s", cfg.Address)
	}
	if !groupIP.IsMulticast() {
		return nil, fmt.Errorf("not a multicast address: %s", cfg.Address)
	}

	rtpPort := int(multicastPortCounter.Add(2) - 2)
	rtcpPort := rtpPort + 1

	ttl := cfg.TTL
	if ttl <= 0 {
		ttl = 16
	}

	var iface *net.Interface
	if cfg.Interface != "" {
		var err error
		iface, err = net.InterfaceByName(cfg.Interface)
		if err != nil {
			return nil, fmt.Errorf("interface %s: %w", cfg.Interface, err)
		}
	}

	rtpAddr := &net.UDPAddr{IP: groupIP, Port: rtpPort}
	rtcpAddr := &net.UDPAddr{IP: groupIP, Port: rtcpPort}

	rtpConn, err := setupMulticastSender(iface, ttl)
	if err != nil {
		return nil, fmt.Errorf("multicast RTP sender: %w", err)
	}

	rtcpConn, err := setupMulticastSender(iface, ttl)
	if err != nil {
		rtpConn.Close()
		return nil, fmt.Errorf("multicast RTCP sender: %w", err)
	}

	return &MulticastTransport{
		rtpConn:  rtpConn,
		rtcpConn: rtcpConn,
		rtpAddr:  rtpAddr,
		rtcpAddr: rtcpAddr,
		rtpPort:  rtpPort,
		rtcpPort: rtcpPort,
		done:     make(chan struct{}),
	}, nil
}

func setupMulticastSender(iface *net.Interface, ttl int) (net.PacketConn, error) {
	conn, err := net.ListenPacket("udp4", "0.0.0.0:0")
	if err != nil {
		return nil, err
	}

	p := ipv4.NewPacketConn(conn)
	if err := p.SetMulticastTTL(ttl); err != nil {
		conn.Close()
		return nil, fmt.Errorf("set TTL: %w", err)
	}
	if iface != nil {
		if err := p.SetMulticastInterface(iface); err != nil {
			conn.Close()
			return nil, fmt.Errorf("set interface: %w", err)
		}
	}

	return conn, nil
}

// SendRTP sends an RTP packet to the multicast group.
func (m *MulticastTransport) SendRTP(data []byte) error {
	_, err := m.rtpConn.WriteTo(data, m.rtpAddr)
	return err
}

// SendRTCP sends an RTCP packet to the multicast group.
func (m *MulticastTransport) SendRTCP(data []byte) error {
	_, err := m.rtcpConn.WriteTo(data, m.rtcpAddr)
	return err
}

// ServerPorts returns the multicast RTP and RTCP ports.
func (m *MulticastTransport) ServerPorts() (int, int) {
	return m.rtpPort, m.rtcpPort
}

// MulticastAddr returns the multicast group IP address.
func (m *MulticastTransport) MulticastAddr() net.IP {
	return m.rtpAddr.IP
}

// Close shuts down the multicast transport.
func (m *MulticastTransport) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	m.closed = true
	close(m.done)
	m.rtpConn.Close()
	m.rtcpConn.Close()
}
