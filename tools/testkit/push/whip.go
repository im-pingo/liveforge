package push

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/im-pingo/liveforge/pkg/avframe"
	pkgrtp "github.com/im-pingo/liveforge/pkg/rtp"
	"github.com/im-pingo/liveforge/tools/testkit/report"
	"github.com/im-pingo/liveforge/tools/testkit/source"
	"github.com/pion/webrtc/v4"
)

const (
	whipConnectTimeout = 10 * time.Second
)

// whipPusher implements Pusher for the WebRTC WHIP protocol. It creates a
// PeerConnection, negotiates via HTTP POST to the WHIP endpoint, and sends
// RTP-packetized H.264 video frames over a WebRTC media track.
type whipPusher struct{}

// Push creates a WebRTC PeerConnection, performs WHIP signaling with the target
// endpoint, and sends H.264 video frames as RTP packets. Audio frames are
// skipped because the server only supports Opus while the source emits AAC.
func (p *whipPusher) Push(ctx context.Context, src source.Source, cfg PushConfig) (*report.PushReport, error) {
	start := time.Now()
	var framesSent int64
	var bytesSent int64

	// Create PeerConnection with no ICE servers (ICE Lite localhost).
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return buildPushReport(cfg, start, framesSent, bytesSent),
			fmt.Errorf("whip: create peer connection: %w", err)
	}
	defer pc.Close()

	// Add H264 video track.
	track, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264},
		"video", "lf-test",
	)
	if err != nil {
		return buildPushReport(cfg, start, framesSent, bytesSent),
			fmt.Errorf("whip: create video track: %w", err)
	}
	if _, err := pc.AddTrack(track); err != nil {
		return buildPushReport(cfg, start, framesSent, bytesSent),
			fmt.Errorf("whip: add track: %w", err)
	}

	// Set up connection state callback before signaling.
	connected := make(chan struct{})
	var connOnce sync.Once
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateConnected {
			connOnce.Do(func() { close(connected) })
		}
	})

	// Create offer and set as local description.
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return buildPushReport(cfg, start, framesSent, bytesSent),
			fmt.Errorf("whip: create offer: %w", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		return buildPushReport(cfg, start, framesSent, bytesSent),
			fmt.Errorf("whip: set local description: %w", err)
	}

	// Wait for ICE gathering to complete so the offer contains candidates.
	gatherDone := webrtc.GatheringCompletePromise(pc)
	select {
	case <-gatherDone:
	case <-ctx.Done():
		return buildPushReport(cfg, start, framesSent, bytesSent), ctx.Err()
	case <-time.After(whipConnectTimeout):
		return buildPushReport(cfg, start, framesSent, bytesSent),
			fmt.Errorf("whip: ICE gathering timeout")
	}

	// POST the offer SDP to the WHIP endpoint.
	answerSDP, err := whipSignal(ctx, cfg.Target, cfg.Token, pc.LocalDescription().SDP)
	if err != nil {
		return buildPushReport(cfg, start, framesSent, bytesSent), err
	}

	// Set remote description with the answer.
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  answerSDP,
	}); err != nil {
		return buildPushReport(cfg, start, framesSent, bytesSent),
			fmt.Errorf("whip: set remote description: %w", err)
	}

	// Wait for the connection to be established.
	select {
	case <-connected:
	case <-ctx.Done():
		return buildPushReport(cfg, start, framesSent, bytesSent), ctx.Err()
	case <-time.After(whipConnectTimeout):
		return buildPushReport(cfg, start, framesSent, bytesSent),
			fmt.Errorf("whip: connection timeout")
	}

	// Create H264 packetizer and RTP session for timestamp/sequence management.
	packetizer, err := pkgrtp.NewPacketizer(avframe.CodecH264)
	if err != nil {
		return buildPushReport(cfg, start, framesSent, bytesSent),
			fmt.Errorf("whip: create packetizer: %w", err)
	}
	session := pkgrtp.NewSession(106, 90000) // H264 PT=106, 90kHz clock

	// Determine deadline from cfg.Duration.
	var deadline time.Time
	if cfg.Duration > 0 {
		deadline = start.Add(cfg.Duration)
	}

	// Frame loop: read frames from source and send as RTP.
	for {
		select {
		case <-ctx.Done():
			return buildPushReport(cfg, start, framesSent, bytesSent), ctx.Err()
		default:
		}

		if !deadline.IsZero() && time.Now().After(deadline) {
			break
		}

		frame, err := src.NextFrame()
		if err == io.EOF {
			break
		}
		if err != nil {
			return buildPushReport(cfg, start, framesSent, bytesSent),
				fmt.Errorf("whip: read source frame: %w", err)
		}

		// Skip audio frames entirely (server only supports Opus, source has AAC).
		if frame.MediaType.IsAudio() {
			continue
		}

		// Skip non-video frames.
		if !frame.MediaType.IsVideo() {
			continue
		}

		// Packetize the video frame (including sequence headers with SPS/PPS).
		rtpPackets, err := packetizer.Packetize(frame, pkgrtp.DefaultMTU)
		if err != nil {
			continue
		}
		if len(rtpPackets) == 0 {
			continue
		}

		// Fill in RTP header fields (SSRC, PT, timestamp, sequence number).
		session.WrapPackets(rtpPackets, frame.DTS)

		// Write each RTP packet to the track. We marshal to bytes and use
		// track.Write because pion/webrtc/v4 uses pion/rtp v1 while our
		// packetizer uses pion/rtp/v2.
		for _, pkt := range rtpPackets {
			raw, err := pkt.Marshal()
			if err != nil {
				return buildPushReport(cfg, start, framesSent, bytesSent),
					fmt.Errorf("whip: marshal RTP: %w", err)
			}
			if _, err := track.Write(raw); err != nil {
				return buildPushReport(cfg, start, framesSent, bytesSent),
					fmt.Errorf("whip: write RTP: %w", err)
			}
			bytesSent += int64(len(raw))
		}

		framesSent++
	}

	return buildPushReport(cfg, start, framesSent, bytesSent), nil
}

// whipSignal sends the SDP offer to the WHIP endpoint via HTTP POST and returns
// the SDP answer. Expects HTTP 201 with Content-Type application/sdp.
func whipSignal(ctx context.Context, target, token, offerSDP string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader([]byte(offerSDP)))
	if err != nil {
		return "", fmt.Errorf("whip: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/sdp")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	client := &http.Client{Timeout: whipConnectTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("whip: POST signal: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("whip: read response body: %w", err)
	}

	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("whip: unexpected status %d: %s", resp.StatusCode, string(body))
	}

	return string(body), nil
}
