package cluster

import (
	"net/url"
	"sort"
	"time"
)

const (
	maxClusterStatusRelays = 100
	maxClusterStatusPeers  = 100
)

// ClusterStatusProvider exposes a bounded, point-in-time cluster snapshot.
type ClusterStatusProvider interface {
	ClusterStatus() ClusterStatus
}

// ClusterStatus is the management-plane view of cluster relays and peer health.
type ClusterStatus struct {
	ActiveForwards int           `json:"active_forwards"`
	ActiveOrigins  int           `json:"active_origins"`
	Relays         []RelayStatus `json:"relays"`
	Peers          []PeerStatus  `json:"peers"`
	Truncated      bool          `json:"truncated"`
}

// RelayStatus describes an active relay without exposing endpoint credentials
// or query parameters.
type RelayStatus struct {
	Direction string    `json:"direction"`
	Protocol  string    `json:"protocol"`
	StreamKey string    `json:"stream_key"`
	Endpoint  string    `json:"endpoint"`
	StartedAt time.Time `json:"started_at"`
}

// PeerStatus describes the bounded health state of a cluster peer.
type PeerStatus struct {
	Host                string    `json:"host"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
	LastAttempt         time.Time `json:"last_attempt,omitempty"`
	LastSuccess         time.Time `json:"last_success,omitempty"`
	Evicted             bool      `json:"evicted"`
}

// ClusterStatus returns a race-safe, bounded status snapshot.
func (m *Module) ClusterStatus() ClusterStatus {
	status := ClusterStatus{
		Relays: make([]RelayStatus, 0),
		Peers:  make([]PeerStatus, 0),
	}

	if m.forward != nil {
		forward := m.forward.StatusSnapshot()
		status.ActiveForwards = len(forward)
		status.Relays = append(status.Relays, forward...)
	}
	if m.origin != nil {
		origin := m.origin.StatusSnapshot()
		status.ActiveOrigins = len(origin)
		status.Relays = append(status.Relays, origin...)
	}

	sort.Slice(status.Relays, func(i, j int) bool {
		if status.Relays[i].Direction != status.Relays[j].Direction {
			return status.Relays[i].Direction < status.Relays[j].Direction
		}
		if status.Relays[i].StreamKey != status.Relays[j].StreamKey {
			return status.Relays[i].StreamKey < status.Relays[j].StreamKey
		}
		return status.Relays[i].Endpoint < status.Relays[j].Endpoint
	})
	if len(status.Relays) > maxClusterStatusRelays {
		status.Relays = status.Relays[:maxClusterStatusRelays]
		status.Truncated = true
	}

	if m.health != nil {
		nodes := m.health.Snapshot()
		hosts := make([]string, 0, len(nodes))
		for host := range nodes {
			hosts = append(hosts, host)
		}
		sort.Strings(hosts)
		if len(hosts) > maxClusterStatusPeers {
			hosts = hosts[:maxClusterStatusPeers]
			status.Truncated = true
		}
		for _, host := range hosts {
			node := nodes[host]
			status.Peers = append(status.Peers, PeerStatus{
				Host:                host,
				ConsecutiveFailures: node.ConsecutiveFailures,
				LastAttempt:         node.LastAttempt,
				LastSuccess:         node.LastSuccess,
				Evicted:             node.Evicted,
			})
		}
	}

	return status
}

func statusEndpoint(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}
