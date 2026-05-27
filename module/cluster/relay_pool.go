package cluster

import (
	"context"
	"fmt"
	"sync"
)

// RelayPool limits concurrent relay connections per peer host.
// This prevents overwhelming a single cluster node when many streams
// need to be forwarded or pulled simultaneously.
type RelayPool struct {
	maxPerHost int
	mu         sync.Mutex
	hosts      map[string]chan struct{}
}

// NewRelayPool creates a relay pool with the given per-host concurrency limit.
func NewRelayPool(maxPerHost int) *RelayPool {
	if maxPerHost <= 0 {
		maxPerHost = 10
	}
	return &RelayPool{
		maxPerHost: maxPerHost,
		hosts:      make(map[string]chan struct{}),
	}
}

// Acquire blocks until a relay slot is available for the given host,
// or the context is cancelled.
func (p *RelayPool) Acquire(ctx context.Context, host string) error {
	sem := p.getSemaphore(host)
	select {
	case sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("relay pool acquire %s: %w", host, ctx.Err())
	}
}

// Release returns a relay slot for the given host.
func (p *RelayPool) Release(host string) {
	sem := p.getSemaphore(host)
	select {
	case <-sem:
	default:
	}
}

// ActiveCount returns the number of active relay connections for a host.
func (p *RelayPool) ActiveCount(host string) int {
	p.mu.Lock()
	sem, ok := p.hosts[host]
	p.mu.Unlock()
	if !ok {
		return 0
	}
	return len(sem)
}

func (p *RelayPool) getSemaphore(host string) chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	sem, ok := p.hosts[host]
	if !ok {
		sem = make(chan struct{}, p.maxPerHost)
		p.hosts[host] = sem
	}
	return sem
}
