package dvr

import (
	"os"
	"sort"
	"sync"
	"time"
)

// Segment represents a single TS segment on disk.
type Segment struct {
	SeqNum    int
	StartTime time.Time
	StartDTS  int64
	Duration  float64
	Filename  string
	Size      int64
	DiskPath  string
}

// SegmentIndex is a thread-safe ordered list of segments.
type SegmentIndex struct {
	mu       sync.RWMutex
	segments []Segment
}

type CleanupResult struct {
	Segments []Segment
	Deleted  uint64
	Bytes    uint64
	Failures uint64
}

// NewSegmentIndex creates a new segment index.
func NewSegmentIndex() *SegmentIndex {
	return &SegmentIndex{}
}

// Add appends a segment to the index.
func (idx *SegmentIndex) Add(seg Segment) {
	idx.mu.Lock()
	idx.segments = append(idx.segments, seg)
	idx.mu.Unlock()
}

// Segments returns a copy of all segments.
func (idx *SegmentIndex) Segments() []Segment {
	idx.mu.RLock()
	out := make([]Segment, len(idx.segments))
	copy(out, idx.segments)
	idx.mu.RUnlock()
	return out
}

// SegmentBySeqNum finds a segment by sequence number using binary search.
func (idx *SegmentIndex) SegmentBySeqNum(seqNum int) (Segment, bool) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	i := sort.Search(len(idx.segments), func(i int) bool {
		return idx.segments[i].SeqNum >= seqNum
	})
	if i < len(idx.segments) && idx.segments[i].SeqNum == seqNum {
		return idx.segments[i], true
	}
	return Segment{}, false
}

// CleanBefore removes segments with StartTime before cutoff and deletes their disk files.
func (idx *SegmentIndex) CleanBefore(cutoff time.Time) []Segment {
	return idx.CleanBeforeWithResult(cutoff).Segments
}

// CleanBeforeWithResult deletes expired files and only removes successfully
// deleted entries from the index. Failed deletions remain visible for retry.
func (idx *SegmentIndex) CleanBeforeWithResult(cutoff time.Time) CleanupResult {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	result := CleanupResult{}
	remaining := make([]Segment, 0, len(idx.segments))
	for _, seg := range idx.segments {
		if !seg.StartTime.Before(cutoff) {
			remaining = append(remaining, seg)
			continue
		}
		err := os.Remove(seg.DiskPath)
		if err != nil && !os.IsNotExist(err) {
			result.Failures++
			remaining = append(remaining, seg)
			continue
		}
		result.Segments = append(result.Segments, seg)
		result.Deleted++
		if seg.Size > 0 {
			result.Bytes += uint64(seg.Size)
		}
	}
	idx.segments = remaining
	return result
}

// Len returns the number of segments.
func (idx *SegmentIndex) Len() int {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return len(idx.segments)
}

// First returns the first (oldest) segment.
func (idx *SegmentIndex) First() (Segment, bool) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	if len(idx.segments) == 0 {
		return Segment{}, false
	}
	return idx.segments[0], true
}

// Last returns the last (newest) segment.
func (idx *SegmentIndex) Last() (Segment, bool) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	if len(idx.segments) == 0 {
		return Segment{}, false
	}
	return idx.segments[len(idx.segments)-1], true
}

// MaxDuration returns the maximum segment duration.
func (idx *SegmentIndex) MaxDuration() float64 {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	var max float64
	for _, seg := range idx.segments {
		if seg.Duration > max {
			max = seg.Duration
		}
	}
	return max
}
