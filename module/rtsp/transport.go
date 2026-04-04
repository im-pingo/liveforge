package rtsp

import (
	"fmt"
	"net"
	"sync"

	"github.com/im-pingo/liveforge/pkg/portalloc"
)

// UDPTransport manages a pair of UDP sockets for RTP and RTCP.
type UDPTransport struct {
	rtpConn    *net.UDPConn
	rtcpConn   *net.UDPConn
	rtpPort    int
	rtcpPort   int
	clientAddr *net.UDPAddr // client's RTP address
	clientRTCP *net.UDPAddr // client's RTCP address
	ports      *portalloc.PortAllocator
	done       chan struct{}
	closed     bool
	mu         sync.Mutex
}

// NewUDPTransport allocates a port pair and binds UDP sockets.
func NewUDPTransport(ports *portalloc.PortAllocator) (*UDPTransport, error) {
	rtpPort, rtcpPort, err := ports.AllocatePair()
	if err != nil {
		return nil, err
	}

	rtpAddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf(":%d", rtpPort))
	if err != nil {
		ports.Free(rtpPort, rtcpPort)
		return nil, fmt.Errorf("resolve RTP addr: %w", err)
	}
	rtpConn, err := net.ListenUDP("udp", rtpAddr)
	if err != nil {
		ports.Free(rtpPort, rtcpPort)
		return nil, fmt.Errorf("listen RTP on :%d: %w", rtpPort, err)
	}

	rtcpAddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf(":%d", rtcpPort))
	if err != nil {
		rtpConn.Close()
		ports.Free(rtpPort, rtcpPort)
		return nil, fmt.Errorf("resolve RTCP addr: %w", err)
	}
	rtcpConn, err := net.ListenUDP("udp", rtcpAddr)
	if err != nil {
		rtpConn.Close()
		ports.Free(rtpPort, rtcpPort)
		return nil, fmt.Errorf("listen RTCP on :%d: %w", rtcpPort, err)
	}

	return &UDPTransport{
		rtpConn:  rtpConn,
		rtcpConn: rtcpConn,
		rtpPort:  rtpPort,
		rtcpPort: rtcpPort,
		ports:    ports,
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
	u.ports.Free(u.rtpPort, u.rtcpPort)
}
