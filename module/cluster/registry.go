// module/cluster/registry.go
package cluster

import (
	"fmt"
	"strings"
	"sync"
)

// TransportRegistry manages protocol transport plugins.
// Transports are registered by URL scheme and resolved at runtime.
type TransportRegistry struct {
	mu         sync.RWMutex
	transports map[string]RelayTransport
}

// NewTransportRegistry creates an empty registry.
func NewTransportRegistry() *TransportRegistry {
	return &TransportRegistry{
		transports: make(map[string]RelayTransport),
	}
}

// Register adds a transport plugin. Overwrites any existing transport
// for the same scheme.
func (r *TransportRegistry) Register(t RelayTransport) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.transports[t.Scheme()] = t
}

// Resolve finds the transport for the given URL's scheme.
func (r *TransportRegistry) Resolve(rawURL string) (RelayTransport, error) {
	scheme := extractScheme(rawURL)
	if scheme == "" {
		return nil, fmt.Errorf("no scheme in URL: %q", rawURL)
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	t, ok := r.transports[scheme]
	if !ok {
		return nil, fmt.Errorf("unsupported relay scheme: %q", scheme)
	}
	return t, nil
}

// Close closes all registered transports.
func (r *TransportRegistry) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range r.transports {
		t.Close()
	}
}

// extractScheme returns the scheme portion of a URL (before "://").
func extractScheme(rawURL string) string {
	idx := strings.Index(rawURL, "://")
	if idx <= 0 {
		return ""
	}
	return rawURL[:idx]
}
