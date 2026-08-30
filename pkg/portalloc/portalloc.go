// Package portalloc provides a thread-safe port pool for RTP/RTCP allocation.
package portalloc

import (
	"fmt"
	"net"
	"sync"
)

// PortAllocator manages a pool of network ports.
type PortAllocator struct {
	mu      sync.Mutex
	used    map[int]bool
	minPort int
	maxPort int
}

// BoundUDPPair is an allocated RTP/RTCP pair with both UDP sockets already
// bound. The caller owns the sockets and must close them before freeing the
// ports in the allocator.
type BoundUDPPair struct {
	RTPPort  int
	RTCPPort int
	RTPConn  *net.UDPConn
	RTCPConn *net.UDPConn
}

// New creates a PortAllocator for the range [minPort, maxPort].
func New(minPort, maxPort int) (*PortAllocator, error) {
	if minPort < 1 || maxPort > 65535 {
		return nil, fmt.Errorf("port range out of bounds: %d-%d", minPort, maxPort)
	}
	if minPort > maxPort {
		return nil, fmt.Errorf("invalid port range: min %d > max %d", minPort, maxPort)
	}
	return &PortAllocator{
		used:    make(map[int]bool),
		minPort: minPort,
		maxPort: maxPort,
	}, nil
}

// Allocate returns a single available port.
func (pa *PortAllocator) Allocate() (int, error) {
	pa.mu.Lock()
	defer pa.mu.Unlock()
	for p := pa.minPort; p <= pa.maxPort; p++ {
		if !pa.used[p] {
			pa.used[p] = true
			return p, nil
		}
	}
	return 0, fmt.Errorf("no available ports in range %d-%d", pa.minPort, pa.maxPort)
}

// AllocatePair returns an even RTP port and its odd RTCP companion.
func (pa *PortAllocator) AllocatePair() (rtpPort, rtcpPort int, err error) {
	pa.mu.Lock()
	defer pa.mu.Unlock()
	for p := pa.firstEvenPort(); p <= pa.maxPort-1; p += 2 {
		if !pa.used[p] && !pa.used[p+1] {
			pa.used[p] = true
			pa.used[p+1] = true
			return p, p + 1, nil
		}
	}
	return 0, 0, fmt.Errorf("no available port pairs in range %d-%d", pa.minPort, pa.maxPort)
}

// AllocateBoundUDPPair atomically reserves a pair in the allocator and binds
// both sockets. Ports already occupied outside the allocator are skipped.
func (pa *PortAllocator) AllocateBoundUDPPair(network string, ip net.IP) (*BoundUDPPair, error) {
	pa.mu.Lock()
	defer pa.mu.Unlock()

	var lastBindErr error
	for p := pa.firstEvenPort(); p <= pa.maxPort-1; p += 2 {
		if pa.used[p] || pa.used[p+1] {
			continue
		}
		rtpConn, err := net.ListenUDP(network, &net.UDPAddr{IP: ip, Port: p})
		if err != nil {
			lastBindErr = err
			continue
		}
		rtcpConn, err := net.ListenUDP(network, &net.UDPAddr{IP: ip, Port: p + 1})
		if err != nil {
			lastBindErr = err
			_ = rtpConn.Close()
			continue
		}
		pa.used[p] = true
		pa.used[p+1] = true
		return &BoundUDPPair{
			RTPPort:  p,
			RTCPPort: p + 1,
			RTPConn:  rtpConn,
			RTCPConn: rtcpConn,
		}, nil
	}
	if lastBindErr != nil {
		return nil, fmt.Errorf("no bindable UDP port pairs in range %d-%d: %w", pa.minPort, pa.maxPort, lastBindErr)
	}
	return nil, fmt.Errorf("no available port pairs in range %d-%d", pa.minPort, pa.maxPort)
}

func (pa *PortAllocator) firstEvenPort() int {
	if pa.minPort%2 == 0 {
		return pa.minPort
	}
	return pa.minPort + 1
}

// Free returns one or more ports to the pool.
func (pa *PortAllocator) Free(ports ...int) {
	pa.mu.Lock()
	defer pa.mu.Unlock()
	for _, port := range ports {
		if port >= pa.minPort && port <= pa.maxPort {
			delete(pa.used, port)
		}
	}
}
