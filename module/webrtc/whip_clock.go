package webrtc

import (
	"sync"
	"time"
)

// whipMediaClock maps the independent RTP timestamp spaces of a WHIP
// session onto one monotonic millisecond timeline. RTP timestamps from audio
// and video cannot be compared directly, so the first packet arrival for
// each track establishes that track's offset from the session origin. Later
// samples use the track's RTP clock, which avoids network jitter accumulating
// in the output timeline.
type whipMediaClock struct {
	mu     sync.Mutex
	origin time.Time
	tracks map[uint32]whipTrackClock
}

type whipTrackClock struct {
	baseTimestamp uint32
	firstArrival  time.Time
	clockRate     int64
}

func newWHIPMediaClock() *whipMediaClock {
	return &whipMediaClock{tracks: make(map[uint32]whipTrackClock)}
}

// DTS returns the timestamp in milliseconds for a packet received at now.
// The track key is the RTP SSRC, which is stable for the lifetime of a track.
func (c *whipMediaClock) DTS(trackKey uint32, timestamp uint32, clockRate int64, now time.Time) int64 {
	if clockRate <= 0 {
		clockRate = 90000
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.origin.IsZero() {
		c.origin = now
	}
	track, ok := c.tracks[trackKey]
	if !ok {
		track = whipTrackClock{
			baseTimestamp: timestamp,
			firstArrival:  now,
			clockRate:     clockRate,
		}
		c.tracks[trackKey] = track
	}

	// RTP timestamp subtraction is intentionally performed in uint32 before
	// converting to int32 so normal 32-bit timestamp wraparound is handled.
	rtpDelta := int64(int32(timestamp - track.baseTimestamp))
	return track.firstArrival.Sub(c.origin).Milliseconds() + rtpDelta*1000/track.clockRate
}
