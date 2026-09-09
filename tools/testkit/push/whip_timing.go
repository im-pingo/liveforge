package push

import (
	"context"
	"fmt"
	"time"

	"github.com/pion/rtp"
)

type whipPacingClock interface {
	Now() time.Time
	Wait(context.Context, time.Duration) error
}

type whipWallClock struct{}

func (whipWallClock) Now() time.Time { return time.Now() }

func (whipWallClock) Wait(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type whipRTPWriter interface {
	WriteRTP(*rtp.Packet) error
}

type whipOpusSender struct {
	track     whipRTPWriter
	pacer     *whipRealtimePacer
	baseMedia time.Duration
	sequence  uint16
	timestamp uint32
	nextAudio time.Time
}

func (s *whipOpusSender) Write(ctx context.Context, payloads [][]byte) (sent, sentBytes int64, err error) {
	for _, payload := range payloads {
		durationSamples, ok := whipOpusPacketDurationSamples(payload)
		if !ok {
			return sent, sentBytes, fmt.Errorf("whip: invalid Opus packet duration")
		}
		mediaTime := s.baseMedia + time.Duration(s.timestamp)*time.Second/48000
		if waitErr := s.pacer.wait(ctx, mediaTime, s.nextAudio); waitErr != nil {
			return sent, sentBytes, waitErr
		}
		packet := &rtp.Packet{
			Header:  rtp.Header{Version: 2, SequenceNumber: s.sequence, Timestamp: s.timestamp},
			Payload: payload,
		}
		if writeErr := s.track.WriteRTP(packet); writeErr != nil {
			return sent, sentBytes, fmt.Errorf("whip: write audio RTP: %w", writeErr)
		}
		if s.pacer.enabled {
			// Video can advance the shared timeline past several buffered audio
			// packets. Keep their spacing even when their media times are late.
			s.nextAudio = s.pacer.clock.Now().Add(time.Duration(durationSamples) * time.Second / 48000)
		}
		s.sequence++
		s.timestamp += durationSamples
		sent++
		sentBytes += int64(packet.MarshalSize())
	}
	return sent, sentBytes, nil
}
