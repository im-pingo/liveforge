package gb28181

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/google/uuid"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/muxer/ps"
	"github.com/pion/rtcp"
	pionrtp "github.com/pion/rtp/v2"
)

const gbOutboundMTU = 1200

type outboundMediaSession struct {
	ctx    context.Context
	cancel context.CancelFunc
	stream *core.Stream

	rtpConn    *net.UDPConn
	rtcpConn   *net.UDPConn
	remoteRTP  *net.UDPAddr
	remoteRTCP *net.UDPAddr
	ssrc       uint32
	sequence   uint16

	closeOnce sync.Once
	subOnce   sync.Once
	wg        sync.WaitGroup
	done      chan error
	admitted  bool

	rtpPackets  atomic.Uint64
	rtpBytes    atomic.Uint64
	rtcpPackets atomic.Uint64
	mediaFrames atomic.Uint64
	audioFrames atomic.Uint64
	videoFrames atomic.Uint64
}

func newOutboundMediaSession(stream *core.Stream, rtpPort, rtcpPort int) (*outboundMediaSession, error) {
	rtpConn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: rtpPort})
	if err != nil {
		return nil, err
	}
	rtcpConn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: rtcpPort})
	if err != nil {
		_ = rtpConn.Close()
		return nil, err
	}
	var random [4]byte
	if _, err := rand.Read(random[:]); err != nil {
		_ = rtpConn.Close()
		_ = rtcpConn.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &outboundMediaSession{
		ctx:      ctx,
		cancel:   cancel,
		stream:   stream,
		rtpConn:  rtpConn,
		rtcpConn: rtcpConn,
		ssrc:     binary.BigEndian.Uint32(random[:]),
		done:     make(chan error, 1),
	}, nil
}

func (s *outboundMediaSession) setRemote(address *net.UDPAddr) error {
	if address == nil || address.IP == nil || address.Port <= 0 || address.Port >= 65535 {
		return errors.New("invalid GB28181 outbound media address")
	}
	s.remoteRTP = address
	s.remoteRTCP = &net.UDPAddr{IP: address.IP, Port: address.Port + 1}
	return nil
}

func (s *outboundMediaSession) admit() error {
	if err := s.stream.AddSubscriber("gb28181"); err != nil {
		return err
	}
	s.admitted = true
	return nil
}

func (s *outboundMediaSession) start() {
	s.wg.Add(1)
	go s.run()
}

func (s *outboundMediaSession) run() {
	err := s.runMedia()
	s.releaseSubscriber()
	s.done <- err
	s.wg.Done()
}

func (s *outboundMediaSession) runMedia() error {
	reader := s.stream.RingBuffer().NewReaderAt(s.stream.GOPCacheSourceStart())
	muxer := ps.NewMuxer()
	for {
		frame, ok := reader.ReadContext(s.ctx)
		if !ok {
			if s.ctx.Err() != nil {
				return nil
			}
			return errors.New("GB28181 outbound media source ended")
		}
		if frame == nil ||
			(frame.MediaType == avframe.MediaTypeVideo && frame.Codec != avframe.CodecH264) ||
			(frame.MediaType == avframe.MediaTypeAudio && frame.Codec != avframe.CodecG711A) ||
			(!frame.MediaType.IsVideo() && !frame.MediaType.IsAudio()) {
			continue
		}
		if err := s.sendFrame(muxer, frame); err != nil {
			if s.ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("GB28181 outbound media send: %w", err)
		}
	}
}

func (s *outboundMediaSession) sendFrame(muxer *ps.Muxer, frame *avframe.AVFrame) error {
	data, err := muxer.Pack(frame)
	if err != nil {
		return err
	}
	timestamp := uint32(frame.DTS * 90)
	for offset := 0; offset < len(data); {
		end := offset + gbOutboundMTU
		if end > len(data) {
			end = len(data)
		}
		packet := &pionrtp.Packet{Header: pionrtp.Header{
			Version:        2,
			PayloadType:    labRTPPayloadType,
			SequenceNumber: s.sequence,
			Timestamp:      timestamp,
			SSRC:           s.ssrc,
			Marker:         end == len(data),
		}, Payload: data[offset:end]}
		encoded, err := packet.Marshal()
		if err != nil {
			return err
		}
		n, err := s.rtpConn.WriteToUDP(encoded, s.remoteRTP)
		if err != nil {
			return err
		}
		s.sequence++
		s.rtpPackets.Add(1)
		s.rtpBytes.Add(uint64(n))
		offset = end
	}
	s.mediaFrames.Add(1)
	if frame.MediaType.IsAudio() {
		s.audioFrames.Add(1)
	} else if frame.MediaType.IsVideo() {
		s.videoFrames.Add(1)
	}
	if s.rtcpPackets.Load() == 0 {
		report := &rtcp.SenderReport{
			SSRC:        s.ssrc,
			NTPTime:     gbNTPTime(time.Now()),
			RTPTime:     timestamp,
			PacketCount: uint32(s.rtpPackets.Load()),
			OctetCount:  uint32(s.rtpBytes.Load()),
		}
		encoded, err := report.Marshal()
		if err == nil {
			if _, err := s.rtcpConn.WriteToUDP(encoded, s.remoteRTCP); err == nil {
				s.rtcpPackets.Add(1)
			}
		}
	}
	return nil
}

func (s *outboundMediaSession) close() {
	s.closeOnce.Do(func() {
		s.cancel()
		_ = s.rtpConn.Close()
		_ = s.rtcpConn.Close()
		s.wg.Wait()
		s.releaseSubscriber()
	})
}

func (s *outboundMediaSession) releaseSubscriber() {
	s.subOnce.Do(func() {
		if s.admitted {
			s.stream.RemoveSubscriber("gb28181")
		}
	})
}

func gbNTPTime(now time.Time) uint64 {
	const ntpEpochOffset = 2208988800
	seconds := uint64(now.Unix() + ntpEpochOffset)
	fraction := uint64(now.Nanosecond()) * (uint64(1) << 32) / uint64(time.Second)
	return seconds<<32 | fraction
}

func (m *Module) startOutboundMedia(ctx context.Context, device *Device, channelID, streamKey string) (*MediaSession, error) {
	stream, ok := m.handler.hub.Find(streamKey)
	if !ok || stream.Publisher() == nil || stream.Publisher().MediaInfo() == nil {
		return nil, fmt.Errorf("%w: receive stream %q", ErrLabInvalidRequest, streamKey)
	}
	mediaInfo := stream.Publisher().MediaInfo()
	if mediaInfo.VideoCodec != avframe.CodecH264 || mediaInfo.AudioCodec != avframe.CodecG711A {
		return nil, fmt.Errorf("%w: receive stream requires H.264 and G.711A", ErrLabInvalidRequest)
	}

	rtpPort, rtcpPort, err := m.handler.ports.AllocatePair()
	if err != nil {
		return nil, err
	}
	portsOwned := true
	defer func() {
		if portsOwned {
			m.handler.ports.Free(rtpPort, rtcpPort)
		}
	}()
	sender, err := newOutboundMediaSession(stream, rtpPort, rtcpPort)
	if err != nil {
		return nil, err
	}
	senderOwned := true
	defer func() {
		if senderOwned {
			sender.close()
		}
	}()
	if err := sender.admit(); err != nil {
		return nil, fmt.Errorf("GB28181 outbound subscriber admission: %w", err)
	}

	remoteIP := extractIP(device.RemoteAddr)
	remotePort := parsePort(device.RemoteAddr)
	request := newGBLabRequest(sip.INVITE, sip.Uri{Scheme: "sip", User: channelID, Host: remoteIP, Port: remotePort}, m.sipService.ServerID(), m.sipService.Domain(), uuid.NewString())
	request.SetBody(buildGBLabSDP(rtpPort, "sendonly"))
	request.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	request.AppendHeader(sip.NewHeader("Subject", fmt.Sprintf("%s:0,%s:0", channelID, m.sipService.ServerID())))
	request.SetTransport("udp")
	dialog, err := sendInvite(ctx, m.sipService, nil, request)
	if err != nil {
		return nil, err
	}
	dialogOwned := true
	defer func() {
		if dialogOwned {
			dialog.Close()
		}
	}()
	select {
	case <-dialog.Done():
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(10 * time.Second):
		return nil, errors.New("GB28181 outbound INVITE timeout")
	}
	response := dialog.Response()
	if response == nil || response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, errors.New("GB28181 outbound INVITE rejected")
	}
	if err := dialog.SendACK(ctx); err != nil {
		terminateAcceptedDialog(dialog)
		return nil, err
	}
	answerPort := parseSDPPort(string(response.Body()))
	if err := sender.setRemote(&net.UDPAddr{IP: net.ParseIP(remoteIP), Port: answerPort}); err != nil {
		terminateAcceptedDialog(dialog)
		return nil, err
	}
	callID := ""
	if header := response.CallID(); header != nil {
		callID = header.Value()
	}
	if callID == "" {
		callID = uuid.NewString()
	}
	session := &MediaSession{
		ID:         callID,
		DeviceID:   device.DeviceID,
		ChannelID:  channelID,
		StreamKey:  streamKey,
		Direction:  SessionDirectionOutbound,
		LocalPort:  rtpPort,
		RemoteAddr: sender.remoteRTP,
		Transport:  device.Transport,
		State:      SessionStateStreaming,
		Sender:     sender,
		Stream:     stream,
		InviteTx:   dialog,
	}
	m.sessions.Add(session)
	sender.start()
	portsOwned = false
	senderOwned = false
	dialogOwned = false
	return session, nil
}
