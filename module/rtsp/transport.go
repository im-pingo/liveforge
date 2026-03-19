package rtsp

import (
	"fmt"
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
