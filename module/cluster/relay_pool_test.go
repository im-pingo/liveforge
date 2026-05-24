package cluster

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestRelayPoolAcquireRelease(t *testing.T) {
	pool := NewRelayPool(2)

	ctx := context.Background()
	if err := pool.Acquire(ctx, "host1:1935"); err != nil {
		t.Fatalf("Acquire 1: %v", err)
	}
	if err := pool.Acquire(ctx, "host1:1935"); err != nil {
		t.Fatalf("Acquire 2: %v", err)
	}

	if pool.ActiveCount("host1:1935") != 2 {
		t.Errorf("ActiveCount = %d, want 2", pool.ActiveCount("host1:1935"))
	}

	pool.Release("host1:1935")
	if pool.ActiveCount("host1:1935") != 1 {
		t.Errorf("ActiveCount after release = %d, want 1", pool.ActiveCount("host1:1935"))
	}

	pool.Release("host1:1935")
	if pool.ActiveCount("host1:1935") != 0 {
		t.Errorf("ActiveCount after full release = %d, want 0", pool.ActiveCount("host1:1935"))
	}
}

func TestRelayPoolBlocksAtLimit(t *testing.T) {
	pool := NewRelayPool(1)

	ctx := context.Background()
	if err := pool.Acquire(ctx, "host1:1935"); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	ctx2, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := pool.Acquire(ctx2, "host1:1935")
	if err == nil {
		t.Error("expected error when pool is full and context expires")
	}
}

func TestRelayPoolUnblocksOnRelease(t *testing.T) {
	pool := NewRelayPool(1)

	ctx := context.Background()
	pool.Acquire(ctx, "host1:1935")

	acquired := make(chan struct{})
	go func() {
		pool.Acquire(context.Background(), "host1:1935")
		close(acquired)
	}()

	time.Sleep(20 * time.Millisecond)
	pool.Release("host1:1935")

	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Error("second Acquire did not unblock after Release")
	}
}

func TestRelayPoolIndependentHosts(t *testing.T) {
	pool := NewRelayPool(1)
	ctx := context.Background()

	if err := pool.Acquire(ctx, "host1:1935"); err != nil {
		t.Fatalf("Acquire host1: %v", err)
	}
	if err := pool.Acquire(ctx, "host2:1935"); err != nil {
		t.Fatalf("Acquire host2: %v", err)
	}

	if pool.ActiveCount("host1:1935") != 1 {
		t.Errorf("host1 ActiveCount = %d, want 1", pool.ActiveCount("host1:1935"))
	}
	if pool.ActiveCount("host2:1935") != 1 {
		t.Errorf("host2 ActiveCount = %d, want 1", pool.ActiveCount("host2:1935"))
	}
}

func TestRelayPoolConcurrentAccess(t *testing.T) {
	pool := NewRelayPool(10)
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pool.Acquire(ctx, "host:1935")
			time.Sleep(time.Millisecond)
			pool.Release("host:1935")
		}()
	}
	wg.Wait()

	if pool.ActiveCount("host:1935") != 0 {
		t.Errorf("ActiveCount = %d, want 0 after all released", pool.ActiveCount("host:1935"))
	}
}

func TestRelayPoolDefaultMaxPerHost(t *testing.T) {
	pool := NewRelayPool(0)
	if pool.maxPerHost != 10 {
		t.Errorf("maxPerHost = %d, want 10 (default)", pool.maxPerHost)
	}
}

func TestRelayPoolUnknownHostActiveCount(t *testing.T) {
	pool := NewRelayPool(5)
	if pool.ActiveCount("unknown:1935") != 0 {
		t.Errorf("ActiveCount for unknown host = %d, want 0", pool.ActiveCount("unknown:1935"))
	}
}

func TestRelayPoolReleaseWithoutAcquire(t *testing.T) {
	pool := NewRelayPool(5)
	pool.Release("host:1935")
	if pool.ActiveCount("host:1935") != 0 {
		t.Errorf("ActiveCount = %d, want 0", pool.ActiveCount("host:1935"))
	}
}
