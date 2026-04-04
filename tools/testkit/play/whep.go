package play

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/im-pingo/liveforge/pkg/avframe"
	pkgrtp "github.com/im-pingo/liveforge/pkg/rtp"
	pionrtp "github.com/pion/rtp/v2"
	"github.com/pion/webrtc/v4"
)

const (
	whepConnectTimeout = 10 * time.Second
)

// whepPlayer implements Player for the WebRTC WHEP protocol. It creates a
// PeerConnection with receive-only transceivers, negotiates via HTTP POST to
// the WHEP endpoint, reads RTP packets from incoming tracks, depacketizes
// them, and delivers AVFrames via the FrameCallback.
type whepPlayer struct{}

// Play creates a WebRTC PeerConnection, performs WHEP signaling with the
// target endpoint, and delivers depacketized media frames via onFrame.
func (p *whepPlayer) Play(ctx context.Context, cfg PlayConfig, onFrame FrameCallback) error {
	// Create PeerConnection with no ICE servers (ICE Lite localhost).
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return fmt.Errorf("whep: create peer connection: %w", err)
	}
	defer pc.Close()

	// Add receive-only transceivers for video and audio.
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionRecvonly,
	}); err != nil {
		return fmt.Errorf("whep: add video transceiver: %w", err)
	}
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionRecvonly,
	}); err != nil {
		return fmt.Errorf("whep: add audio transceiver: %w", err)
	}

	// Set up connection state callback before signaling.
	connected := make(chan struct{})
	var connOnce sync.Once
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateConnected {
			connOnce.Do(func() { close(connected) })
		}
	})

	// Set up OnTrack handler to depacketize incoming RTP and deliver frames.
	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		codec := mimeToCodec(track.Codec().MimeType)
		if codec == 0 {
			return
		}
		dp, err := pkgrtp.NewDepacketizer(codec)
		if err != nil {
			return
		}
		go func() {
			buf := make([]byte, 1500)
			for {
				// pion/webrtc/v4 uses pion/rtp v1, but our depacketizer uses v2.
				// Read raw RTP bytes and unmarshal with v2.
				n, _, readErr := track.Read(buf)
				if readErr != nil {
					return
				}
				var pkt pionrtp.Packet
				if unmarshalErr := pkt.Unmarshal(buf[:n]); unmarshalErr != nil {
					continue
				}
				frame, dpErr := dp.Depacketize(&pkt)
				if dpErr != nil || frame == nil {
					continue
				}
				onFrame(frame)
			}
		}()
	})

	// Create offer and set as local description.
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return fmt.Errorf("whep: create offer: %w", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		return fmt.Errorf("whep: set local description: %w", err)
	}

	// Wait for ICE gathering to complete so the offer contains candidates.
	gatherDone := webrtc.GatheringCompletePromise(pc)
	select {
	case <-gatherDone:
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(whepConnectTimeout):
		return fmt.Errorf("whep: ICE gathering timeout")
	}

	// POST the offer SDP to the WHEP endpoint.
	answerSDP, err := whepSignal(ctx, cfg.URL, cfg.Token, pc.LocalDescription().SDP)
	if err != nil {
		return err
	}

	// Set remote description with the answer.
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  answerSDP,
	}); err != nil {
		return fmt.Errorf("whep: set remote description: %w", err)
	}

	// Wait for the connection to be established.
	select {
	case <-connected:
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(whepConnectTimeout):
		return fmt.Errorf("whep: connection timeout")
	}

	// Wait for the configured duration or context cancellation.
	if cfg.Duration > 0 {
		select {
		case <-time.After(cfg.Duration):
		case <-ctx.Done():
			return ctx.Err()
		}
	} else {
		<-ctx.Done()
	}

	return nil
}

// whepSignal sends the SDP offer to the WHEP endpoint via HTTP POST and returns
// the SDP answer. Expects HTTP 201 Created with Content-Type application/sdp.
func whepSignal(ctx context.Context, target, token, offerSDP string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader([]byte(offerSDP)))
	if err != nil {
		return "", fmt.Errorf("whep: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/sdp")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	client := &http.Client{Timeout: whepConnectTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("whep: POST signal: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("whep: read response body: %w", err)
	}

	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("whep: unexpected status %d: %s", resp.StatusCode, string(body))
	}

	return string(body), nil
}

// mimeToCodec maps WebRTC codec MIME types to avframe codec types.
func mimeToCodec(mime string) avframe.CodecType {
	switch strings.ToLower(mime) {
	case "video/h264":
		return avframe.CodecH264
	case "video/h265":
		return avframe.CodecH265
	case "video/vp8":
		return avframe.CodecVP8
	case "video/vp9":
		return avframe.CodecVP9
	case "video/av1":
		return avframe.CodecAV1
	case "audio/opus":
		return avframe.CodecOpus
	case "audio/pcmu":
		return avframe.CodecG711U
	case "audio/pcma":
		return avframe.CodecG711A
	default:
		return 0
	}
}
