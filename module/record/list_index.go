package record

import (
	"context"
	"os"
	"slices"
	"sync"
	"time"
)

const (
	recordingListMaxAge     = 5 * time.Second
	recordingListMaxEntries = 100_000
	recordingListMaxBytes   = 32 << 20
)

// recordingListIndex retains one immutable snapshot. Filesystem work never
// holds its lock, so recording finalization cannot wait behind a directory walk.
type recordingListIndex struct {
	mu         sync.Mutex
	items      []RecordingInfo
	expires    time.Time
	generation uint64
	scanning   chan struct{}
	closed     bool
}

func (index *recordingListIndex) list(ctx context.Context, scan func(context.Context) ([]RecordingInfo, error)) ([]RecordingInfo, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		index.mu.Lock()
		if index.closed {
			index.mu.Unlock()
			return nil, os.ErrClosed
		}
		if time.Now().Before(index.expires) {
			items := index.items
			index.mu.Unlock()
			return slices.Clone(items), nil
		}
		if done := index.scanning; done != nil {
			index.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-done:
				continue
			}
		}
		generation := index.generation
		started := time.Now()
		done := make(chan struct{})
		index.scanning = done
		index.items = nil
		index.expires = time.Time{}
		index.mu.Unlock()

		items, err := scan(ctx)
		if err == nil {
			err = ctx.Err()
		}
		cacheable := err == nil && recordingListFitsCache(items)
		index.mu.Lock()
		if cacheable && !index.closed && generation == index.generation {
			index.items = slices.Clone(items)
			index.expires = started.Add(recordingListMaxAge)
		}
		closed := index.closed
		index.scanning = nil
		close(done)
		index.mu.Unlock()
		if closed {
			return nil, os.ErrClosed
		}
		return items, err
	}
}

func (index *recordingListIndex) invalidate() {
	index.mu.Lock()
	index.generation++
	index.items = nil
	index.expires = time.Time{}
	index.mu.Unlock()
}

func (index *recordingListIndex) close() {
	index.mu.Lock()
	index.closed = true
	index.items = nil
	index.expires = time.Time{}
	index.mu.Unlock()
}

func recordingListFitsCache(items []RecordingInfo) bool {
	if len(items) > recordingListMaxEntries {
		return false
	}
	// The fixed allowance includes the value, slice allocation overhead, and
	// timestamps; string contents are charged separately to bound large metadata.
	remaining := recordingListMaxBytes - int64(len(items))*256
	for _, item := range items {
		for _, value := range []string{item.ID, item.StreamKey, item.Format, string(item.State), item.Error} {
			remaining -= int64(len(value))
			if remaining < 0 {
				return false
			}
		}
	}
	return true
}
