package cluster

import (
	"log/slog"
	"net"
	"net/url"
	"sync"
	"time"

	"github.com/im-pingo/liveforge/config"
)

// NodeStatus tracks the health state of a single cluster peer.
type NodeStatus struct {
	ConsecutiveFailures int
	LastAttempt         time.Time
	LastSuccess         time.Time
	Evicted             bool
}

// HealthTracker monitors cluster peer health based on relay connection
// outcomes. Nodes that exceed the failure threshold are evicted (excluded
// from target resolution). A background goroutine probes evicted nodes
// and re-admits them when they respond.
type HealthTracker struct {
	mu             sync.RWMutex
	nodes          map[string]*NodeStatus
	evictThreshold int
	probeInterval  time.Duration
	probeTimeout   time.Duration
	closed         chan struct{}
	reload         chan struct{}
}

// NewHealthTracker creates a health tracker from cluster config.
func NewHealthTracker(cfg config.HealthCheckConfig) *HealthTracker {
	evict := cfg.EvictThreshold
	if evict <= 0 {
		evict = 3
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ht := &HealthTracker{
		nodes:          make(map[string]*NodeStatus),
		evictThreshold: evict,
		probeInterval:  interval,
		probeTimeout:   timeout,
		closed:         make(chan struct{}),
		reload:         make(chan struct{}, 1),
	}
	go ht.recoveryLoop()
	return ht
}

// UpdateConfig publishes health thresholds and resets the recovery interval.
func (ht *HealthTracker) UpdateConfig(cfg config.HealthCheckConfig) {
	evict := cfg.EvictThreshold
	if evict <= 0 {
		evict = 3
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ht.mu.Lock()
	ht.evictThreshold = evict
	ht.probeInterval = interval
	ht.probeTimeout = timeout
	ht.mu.Unlock()
	select {
	case ht.reload <- struct{}{}:
	default:
	}
}

func (ht *HealthTracker) evictThresholdValue() int {
	ht.mu.RLock()
	defer ht.mu.RUnlock()
	return ht.evictThreshold
}

// RecordSuccess records a successful relay connection to the given URL.
func (ht *HealthTracker) RecordSuccess(rawURL string) {
	host := extractHost(rawURL)
	if host == "" {
		return
	}
	now := time.Now()

	ht.mu.Lock()
	defer ht.mu.Unlock()

	ns, ok := ht.nodes[host]
	if !ok {
		ht.nodes[host] = &NodeStatus{LastAttempt: now, LastSuccess: now}
		return
	}
	wasEvicted := ns.Evicted
	ns.ConsecutiveFailures = 0
	ns.LastAttempt = now
	ns.LastSuccess = now
	ns.Evicted = false

	if wasEvicted {
		slog.Info("cluster node recovered", "module", "cluster", "host", host)
	}
}

// RecordFailure records a failed relay connection. Returns true if the
// node was just evicted by this call.
func (ht *HealthTracker) RecordFailure(rawURL string) bool {
	host := extractHost(rawURL)
	if host == "" {
		return false
	}
	now := time.Now()

	ht.mu.Lock()
	defer ht.mu.Unlock()

	ns, ok := ht.nodes[host]
	if !ok {
		ns = &NodeStatus{}
		ht.nodes[host] = ns
	}
	ns.ConsecutiveFailures++
	ns.LastAttempt = now

	if !ns.Evicted && ns.ConsecutiveFailures >= ht.evictThreshold {
		ns.Evicted = true
		slog.Warn("cluster node evicted", "module", "cluster",
			"host", host, "failures", ns.ConsecutiveFailures)
		return true
	}
	return false
}

// IsEvicted returns true if the host extracted from rawURL is currently evicted.
func (ht *HealthTracker) IsEvicted(rawURL string) bool {
	host := extractHost(rawURL)
	if host == "" {
		return false
	}
	ht.mu.RLock()
	defer ht.mu.RUnlock()
	if ns, ok := ht.nodes[host]; ok {
		return ns.Evicted
	}
	return false
}

// FilterHealthy returns only the URLs whose hosts are not evicted.
func (ht *HealthTracker) FilterHealthy(urls []string) []string {
	ht.mu.RLock()
	defer ht.mu.RUnlock()

	healthy := make([]string, 0, len(urls))
	for _, u := range urls {
		host := extractHost(u)
		if host == "" {
			healthy = append(healthy, u)
			continue
		}
		if ns, ok := ht.nodes[host]; ok && ns.Evicted {
			continue
		}
		healthy = append(healthy, u)
	}
	return healthy
}

// Snapshot returns a copy of all tracked nodes (for API/diagnostics).
func (ht *HealthTracker) Snapshot() map[string]NodeStatus {
	ht.mu.RLock()
	defer ht.mu.RUnlock()
	out := make(map[string]NodeStatus, len(ht.nodes))
	for k, v := range ht.nodes {
		out[k] = *v
	}
	return out
}

// Close stops the background recovery loop.
func (ht *HealthTracker) Close() {
	select {
	case <-ht.closed:
	default:
		close(ht.closed)
	}
}

// recoveryLoop periodically probes evicted nodes via TCP dial.
func (ht *HealthTracker) recoveryLoop() {
	for {
		ht.mu.RLock()
		interval := ht.probeInterval
		ht.mu.RUnlock()
		timer := time.NewTimer(interval)
		select {
		case <-ht.closed:
			timer.Stop()
			return
		case <-ht.reload:
			timer.Stop()
		case <-timer.C:
			ht.probeEvicted()
		}
	}
}

func (ht *HealthTracker) probeEvicted() {
	ht.mu.RLock()
	timeout := ht.probeTimeout
	var evicted []string
	for host, ns := range ht.nodes {
		if ns.Evicted {
			evicted = append(evicted, host)
		}
	}
	ht.mu.RUnlock()

	for _, host := range evicted {
		conn, err := net.DialTimeout("tcp", host, timeout)
		if err != nil {
			continue
		}
		conn.Close()

		ht.mu.Lock()
		if ns, ok := ht.nodes[host]; ok && ns.Evicted {
			ns.Evicted = false
			ns.ConsecutiveFailures = 0
			ns.LastSuccess = time.Now()
			slog.Info("cluster node recovered via probe", "module", "cluster", "host", host)
		}
		ht.mu.Unlock()
	}
}

// extractHost returns "host:port" from a URL string.
func extractHost(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Host
}
