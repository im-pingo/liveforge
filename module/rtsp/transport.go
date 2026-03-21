package rtsp

import (
	"fmt"
	"net"
	"sync"
)

// PortManager manages UDP port allocation for RTP/RTCP pairs.
type PortManager struct {
	minPort int
	maxPort int
	used    map[int]bool
	mu      sync.Mutex
}

// NewPortManager creates a PortManager that allocates ports within [minPort, maxPort).
func NewPortManager(minPort, maxPort int) *PortManager {
	return &PortManager{
		minPort: minPort,
		maxPort: maxPort,
		used:    make(map[int]bool),
	}
}

// Allocate returns an even RTP port and its odd RTCP companion.
func (pm *PortManager) Allocate() (rtpPort, rtcpPort int, err error) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	for p := pm.minPort; p < pm.maxPort; p += 2 {
		if p%2 != 0 {
			continue
		}
		if !pm.used[p] && !pm.used[p+1] {
			pm.used[p] = true
			pm.used[p+1] = true
			return p, p + 1, nil
		}
	}
	return 0, 0, fmt.Errorf("no available port pairs in range %d-%d", pm.minPort, pm.maxPort)
}

// Release returns a port pair to the pool.
func (pm *PortManager) Release(rtpPort int) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	delete(pm.used, rtpPort)
	delete(pm.used, rtpPort+1)
}

// UDPTransport manages a pair of UDP sockets for RTP and RTCP.
type UDPTransport struct {
	rtpConn    *net.UDPConn
	rtcpConn   *net.UDPConn
	rtpPort    int
	rtcpPort   int
	clientAddr *net.UDPAddr // client's RTP address
	clientRTCP *net.UDPAddr // client's RTCP address
	pm         *PortManager
	done       chan struct{}
	closed     bool
	mu         sync.Mutex
}

// NewUDPTransport allocates a port pair and binds UDP sockets.
func NewUDPTransport(pm *PortManager) (*UDPTransport, error) {
	rtpPort, rtcpPort, err := pm.Allocate()
	if err != nil {
		return nil, err
	}

	rtpAddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf(":%d", rtpPort))
	if err != nil {
		pm.Release(rtpPort)
		return nil, fmt.Errorf("resolve RTP addr: %w", err)
	}
	rtpConn, err := net.ListenUDP("udp", rtpAddr)
	if err != nil {
		pm.Release(rtpPort)
		return nil, fmt.Errorf("listen RTP on :%d: %w", rtpPort, err)
	}

	rtcpAddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf(":%d", rtcpPort))
	if err != nil {
		rtpConn.Close()
		pm.Release(rtpPort)
		return nil, fmt.Errorf("resolve RTCP addr: %w", err)
	}
	rtcpConn, err := net.ListenUDP("udp", rtcpAddr)
	if err != nil {
		rtpConn.Close()
		pm.Release(rtpPort)
		return nil, fmt.Errorf("listen RTCP on :%d: %w", rtcpPort, err)
	}

	return &UDPTransport{
		rtpConn:  rtpConn,
		rtcpConn: rtcpConn,
		rtpPort:  rtpPort,
		rtcpPort: rtcpPort,
		pm:       pm,
		done:     make(chan struct{}),
	}, nil
}

// SetClientAddr sets the remote client's RTP and RTCP addresses.
func (u *UDPTransport) SetClientAddr(ip net.IP, rtpPort, rtcpPort int) {
	u.clientAddr = &net.UDPAddr{IP: ip, Port: rtpPort}
	u.clientRTCP = &net.UDPAddr{IP: ip, Port: rtcpPort}
}

// SendRTP sends an RTP packet to the client.
func (u *UDPTransport) SendRTP(data []byte) error {
	if u.clientAddr == nil {
		return fmt.Errorf("client RTP address not set")
	}
	_, err := u.rtpConn.WriteToUDP(data, u.clientAddr)
	return err
}

// SendRTCP sends an RTCP packet to the client.
func (u *UDPTransport) SendRTCP(data []byte) error {
	if u.clientRTCP == nil {
		return fmt.Errorf("client RTCP address not set")
	}
	_, err := u.rtcpConn.WriteToUDP(data, u.clientRTCP)
	return err
}

// ReadRTP reads an RTP packet from the UDP socket.
func (u *UDPTransport) ReadRTP(buf []byte) (int, *net.UDPAddr, error) {
	return u.rtpConn.ReadFromUDP(buf)
}

// ReadRTCP reads an RTCP packet from the UDP socket.
func (u *UDPTransport) ReadRTCP(buf []byte) (int, *net.UDPAddr, error) {
	return u.rtcpConn.ReadFromUDP(buf)
}

// ServerPorts returns the allocated server RTP and RTCP ports.
func (u *UDPTransport) ServerPorts() (int, int) {
	return u.rtpPort, u.rtcpPort
}

// Close shuts down both UDP sockets and releases the port pair.
func (u *UDPTransport) Close() {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed {
		return
	}
	u.closed = true
	close(u.done)
	u.rtpConn.Close()
	u.rtcpConn.Close()
	u.pm.Release(u.rtpPort)
}
