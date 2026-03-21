package webrtc

import (
	"log"
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
//   - PLI / FIR  → onPLI callback (keyframe request)
//   - NACK       → onNACK callback + stats (retransmission request)
//   - ReceiverReport → onReceiverReport callback + stats (quality metrics)
//
// Note: pion's default interceptors handle NACK retransmission and TWCC
// automatically. TrackSender monitors these for observability; it does NOT
// replace the interceptor-level handling.
//
// Usage:
//
//	ts := NewTrackSender(sessionID, track, sender)
//	ts.SetPLIHandler(func() { /* send keyframe */ })
//	ts.SetNACKHandler(func(nack *rtcp.TransportLayerNack) { /* log/stats */ })
//	ts.Start()
//	ts.WriteSample(media.Sample{...})
type TrackSender struct {
	sessionID        string
	track            *webrtc.TrackLocalStaticSample
	sender           *webrtc.RTPSender
	onPLI            func()                                    // called on PLI or FIR
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
// The callback should respond by sending an IDR keyframe so the decoder
// can recover from corruption without waiting for the next natural keyframe.
//
// The callback is invoked from the RTCP goroutine; it must be goroutine-safe.
func (ts *TrackSender) SetPLIHandler(fn func()) {
	ts.onPLI = fn
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
func (ts *TrackSender) WriteSample(s media.Sample) error {
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
				log.Printf("[webrtc] session %s: PLI received, requesting keyframe", ts.sessionID)
				if ts.onPLI != nil {
					ts.onPLI()
				}

			case *rtcp.FullIntraRequest:
				ts.Stats.PLICount.Add(1)
				log.Printf("[webrtc] session %s: FIR received, requesting keyframe", ts.sessionID)
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
