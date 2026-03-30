package webrtc

import (
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

// TrackSenderStats holds cumulative RTCP statistics for a track.
// All fields are updated atomically from the RTCP goroutine.
type TrackSenderStats struct {
	PLICount  atomic.Uint64 // total PLI/FIR requests received
	NACKCount atomic.Uint64 // total NACK packets received (may be 0 if pion interceptor consumes them)

	// From ReceiverReport
	PacketsLost    atomic.Int64  // cumulative packets lost reported by receiver
	FractionLost   atomic.Uint32 // last reported fraction lost (0-255, where 255 = 100%)
	Jitter         atomic.Uint32 // last reported interarrival jitter (in RTP timestamp units)
	LastSenderSSRC atomic.Uint32 // SSRC of the sender this report applies to
}

// TrackSender wraps a pion RTPSender and its TrackLocalStaticSample.
// It runs a dedicated RTCP read loop that parses incoming RTCP packets
// and dispatches them to registered handlers, decoupling RTCP processing
// from application-layer protocol code (WHEP, future SIP, etc.).
//
// Supported RTCP types:
//   - PLI / FIR  → sets needsKeyframe flag + onPLI callback
//   - NACK       → onNACK callback + stats (retransmission request)
//   - ReceiverReport → onReceiverReport callback + stats (quality metrics)
//
// PLI/FIR handling: Instead of writing media directly from the RTCP goroutine,
// TrackSender sets an atomic needsKeyframe flag. The feed loop checks this flag
// and skips inter-frames until the next keyframe arrives, ensuring only one
// goroutine ever writes media samples. This eliminates timestamp interleaving
// between the RTCP goroutine and the feed loop.
//
// Note: pion's default interceptors handle NACK retransmission and TWCC
// automatically. TrackSender monitors these for observability; it does NOT
// replace the interceptor-level handling.
//
// Usage:
//
//	ts := NewTrackSender(sessionID, track, sender)
//	ts.SetPLIHandler(func() { /* log/stats */ })
//	ts.SetNACKHandler(func(nack *rtcp.TransportLayerNack) { /* log/stats */ })
//	ts.Start()
//	ts.WriteSample(media.Sample{...})
type TrackSender struct {
	sessionID        string
	track            *webrtc.TrackLocalStaticSample
	sender           *webrtc.RTPSender
	mu               sync.Mutex                                // serializes WriteSample calls across goroutines
	needsKeyframe    atomic.Bool                               // set by RTCP loop on PLI/FIR, cleared by feed loop on keyframe
	onPLI            func()                                    // called on PLI or FIR (for logging/stats only)
	onNACK           func(nack *rtcp.TransportLayerNack)       // called on NACK
	onReceiverReport func(report *rtcp.ReceiverReport)         // called on ReceiverReport
	onREMB           func(bitrate uint64, ssrcs []uint32)      // called on REMB
	Stats            TrackSenderStats
}

// NewTrackSender creates a TrackSender. Call Start() to begin RTCP processing.
func NewTrackSender(sessionID string, track *webrtc.TrackLocalStaticSample, sender *webrtc.RTPSender) *TrackSender {
	return &TrackSender{
		sessionID: sessionID,
		track:     track,
		sender:    sender,
	}
}

// SetPLIHandler registers a callback invoked when the remote peer sends a
// PLI (Picture Loss Indication) or FIR (Full Intra Request) RTCP packet.
// The callback is for logging/monitoring only — keyframe recovery is handled
// automatically via the needsKeyframe flag: the feed loop skips inter-frames
// until the next keyframe arrives, ensuring a clean decoder resync.
//
// The callback is invoked from the RTCP goroutine; it must be goroutine-safe.
func (ts *TrackSender) SetPLIHandler(fn func()) {
	ts.onPLI = fn
}

// NeedsKeyframe returns true if a PLI/FIR has been received and the feed loop
// should skip inter-frames until the next keyframe.
func (ts *TrackSender) NeedsKeyframe() bool {
	return ts.needsKeyframe.Load()
}

// ClearNeedsKeyframe resets the flag after the feed loop has sent a keyframe.
func (ts *TrackSender) ClearNeedsKeyframe() {
	ts.needsKeyframe.Store(false)
}

// SetNACKHandler registers a callback invoked when a NACK packet is received.
// Note: pion's NACK responder interceptor handles the actual retransmission;
// this callback is for monitoring/logging purposes.
func (ts *TrackSender) SetNACKHandler(fn func(nack *rtcp.TransportLayerNack)) {
	ts.onNACK = fn
}

// SetReceiverReportHandler registers a callback invoked when a ReceiverReport
// is received. Useful for monitoring packet loss, jitter, and RTT.
func (ts *TrackSender) SetReceiverReportHandler(fn func(report *rtcp.ReceiverReport)) {
	ts.onReceiverReport = fn
}

// SetREMBHandler registers a callback invoked when a REMB (Receiver Estimated
// Maximum Bitrate) packet is received.
func (ts *TrackSender) SetREMBHandler(fn func(bitrate uint64, ssrcs []uint32)) {
	ts.onREMB = fn
}

// WriteSample encodes a raw media sample as RTP packets and sends them to the peer.
// It serializes concurrent callers (feed loop + PLI handler) so that all RTP packets
// for a single sample are sent contiguously, preventing interleaved packet corruption.
func (ts *TrackSender) WriteSample(s media.Sample) error {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.track.WriteSample(s)
}

// Track returns the underlying local track (useful for codec/MIME inspection).
func (ts *TrackSender) Track() *webrtc.TrackLocalStaticSample {
	return ts.track
}

// Start launches the RTCP read loop in a goroutine. The loop exits when
// ReadRTCP returns an error (peer connection closed or session torn down).
func (ts *TrackSender) Start() {
	go ts.rtcpLoop()
}

// rtcpLoop continuously reads RTCP packets from the sender and dispatches
// them to the appropriate handlers. This is the only place in the codebase
// that parses incoming RTCP for a given track — protocol handlers never
// inspect raw RTCP bytes.
func (ts *TrackSender) rtcpLoop() {
	for {
		pkts, _, err := ts.sender.ReadRTCP()
		if err != nil {
			return
		}
		for _, pkt := range pkts {
			switch p := pkt.(type) {
			case *rtcp.PictureLossIndication:
				ts.Stats.PLICount.Add(1)
				ts.needsKeyframe.Store(true)
				slog.Debug("PLI received", "module", "webrtc", "session", ts.sessionID)
				if ts.onPLI != nil {
					ts.onPLI()
				}

			case *rtcp.FullIntraRequest:
				ts.Stats.PLICount.Add(1)
				ts.needsKeyframe.Store(true)
				slog.Debug("FIR received", "module", "webrtc", "session", ts.sessionID)
				if ts.onPLI != nil {
					ts.onPLI()
				}

			case *rtcp.TransportLayerNack:
				ts.Stats.NACKCount.Add(1)
				if ts.onNACK != nil {
					ts.onNACK(p)
				}

			case *rtcp.ReceiverReport:
				for _, rr := range p.Reports {
					ts.Stats.PacketsLost.Store(int64(rr.TotalLost))
					ts.Stats.FractionLost.Store(uint32(rr.FractionLost))
					ts.Stats.Jitter.Store(rr.Jitter)
					ts.Stats.LastSenderSSRC.Store(rr.SSRC)
				}
				if ts.onReceiverReport != nil {
					ts.onReceiverReport(p)
				}

			case *rtcp.ReceiverEstimatedMaximumBitrate:
				if ts.onREMB != nil {
					ts.onREMB(uint64(p.Bitrate), p.SSRCs)
				}
			}
		}
	}
}
