package webrtc

import (
	"log"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

// TrackSender wraps a pion RTPSender and its TrackLocalStaticSample.
// It runs a dedicated RTCP read loop that parses incoming RTCP packets
// and dispatches them to registered handlers, decoupling RTCP processing
// from application-layer protocol code (WHEP, future SIP, etc.).
//
// Usage:
//
//	ts := NewTrackSender(sessionID, track, sender)
//	ts.SetPLIHandler(func() { /* send keyframe */ })
//	ts.Start()
//	ts.WriteSample(media.Sample{...})
type TrackSender struct {
	sessionID string
	track     *webrtc.TrackLocalStaticSample
	sender    *webrtc.RTPSender
	onPLI     func() // called when PLI or FIR received from remote peer
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
			switch pkt.(type) {
			case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
				log.Printf("[webrtc] session %s: PLI/FIR received, requesting keyframe", ts.sessionID)
				if ts.onPLI != nil {
					ts.onPLI()
				}
			}
		}
	}
}
