// Package portalloc provides a thread-safe port pool for RTP/RTCP allocation.
package portalloc

import (
	"fmt"
	"sync"
)

// PortAllocator manages a pool of network ports.
type PortAllocator struct {
	mu      sync.Mutex
	used    map[int]bool
	minPort int
	maxPort int
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
	for p := pa.minPort; p <= pa.maxPort-1; p += 2 {
		if p%2 != 0 {
			continue
		}
		if !pa.used[p] && !pa.used[p+1] {
			pa.used[p] = true
			pa.used[p+1] = true
			return p, p + 1, nil
		}
	}
	return 0, 0, fmt.Errorf("no available port pairs in range %d-%d", pa.minPort, pa.maxPort)
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
