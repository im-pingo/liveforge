// gb28181-sim simulates a GB28181 device (IPC camera) for acceptance testing.
//
// It performs:
//  1. SIP REGISTER with the liveforge SIP server
//  2. Periodic keepalive MESSAGEs
//  3. Responds to catalog queries (MESSAGE with CmdType=Catalog)
//  4. Handles incoming INVITE by sending RTP/PS video+audio (H.264+AAC via ffmpeg)
//  5. Handles BYE to stop streaming
//
// Requires ffmpeg in PATH for generating test media.
//
// Usage:
//
//	go run ./tools/gb28181-sim
//	go run ./tools/gb28181-sim -server 127.0.0.1:5060 -fps 25 -keepalive 10s
package main

import (
	"context"
	"encoding/xml"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/muxer/ps"
	pionrtp "github.com/pion/rtp/v2"
)

var (
	flagServerAddr = flag.String("server", "127.0.0.1:5060", "liveforge SIP server address")
	flagDeviceID   = flag.String("device-id", "34020000001110000001", "device ID")
	flagDomain     = flag.String("domain", "3402000000", "SIP domain")
	flagServerID   = flag.String("server-id", "34020000002000000001", "server device ID")
	flagTransport  = flag.String("transport", "udp", "SIP transport (udp or tcp)")
	flagLocalPort  = flag.Int("local-port", 5061, "local SIP port")
	flagKeepalive  = flag.Duration("keepalive", 30*time.Second, "keepalive interval")
	flagFPS        = flag.Int("fps", 25, "video frame rate for RTP streaming")
	flagNoAudio    = flag.Bool("no-audio", false, "disable audio generation")
)

// channels is the simulated device catalog, populated in main() from the device ID.
var channels []channelInfo

type channelInfo struct {
	ID, Name, Manufacturer, Status string
	PTZType                        int
	Latitude, Longitude            float64
}

// buildChannels generates channel entries derived from the device ID.
// Each simulated device has a single channel whose ID matches the device ID,
// which is the standard pattern for single-channel IPCs in GB28181.
func buildChannels(deviceID string) []channelInfo {
	return []channelInfo{
		{
			ID:           deviceID,
			Name:         "Camera " + deviceID[len(deviceID)-4:],
			Manufacturer: "Hikvision",
			Status:       "ON",
			PTZType:      1,
			Latitude:     39.916527,
			Longitude:    116.397128,
		},
	}
}

// rtpSession tracks an active media session (sending RTP).
type rtpSession struct {
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func main() {
	flag.Parse()

	channels = buildChannels(*flagDeviceID)

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))
	slog.Info("gb28181-sim starting",
		"device_id", *flagDeviceID,
		"server", *flagServerAddr,
		"transport", *flagTransport,
		"local_port", *flagLocalPort,
		"channels", len(channels))

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := run(ctx); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return fmt.Errorf("ffmpeg not found in PATH: %w", err)
	}

	ua, err := sipgo.NewUA(
		sipgo.WithUserAgent("GB28181-Sim"),
		sipgo.WithUserAgentHostname(*flagDomain),
	)
	if err != nil {
		return fmt.Errorf("create UA: %w", err)
	}

	srv, err := sipgo.NewServer(ua)
	if err != nil {
		return fmt.Errorf("create server: %w", err)
	}

	client, err := sipgo.NewClient(ua)
	if err != nil {
		return fmt.Errorf("create client: %w", err)
	}

	var mu sync.Mutex
	sessions := make(map[string]*rtpSession)

	srv.OnMessage(func(req *sip.Request, tx sip.ServerTransaction) {
		body := req.Body()
		cmdType := parseCmdType(body)
		slog.Info("received MESSAGE", "cmd_type", cmdType)

		switch cmdType {
		case "Catalog":
			catalogResp := buildCatalogResponse(body)
			sendMessage(ctx, client, catalogResp)
		case "DeviceControl":
			slog.Info("received PTZ control command", "body", string(body))
		default:
			slog.Debug("unhandled message", "cmd_type", cmdType)
		}

		resp := sip.NewResponseFromRequest(req, 200, "OK", nil)
		tx.Respond(resp)
	})

	srv.OnInvite(func(req *sip.Request, tx sip.ServerTransaction) {
		slog.Info("received INVITE", "from", req.From().Value(), "body_len", len(req.Body()))

		sdp := string(req.Body())
		remotePort := parseSDPPort(sdp)
		remoteIP := extractIP(*flagServerAddr)
		if remotePort == 0 {
			slog.Error("no RTP port in SDP")
			resp := sip.NewResponseFromRequest(req, 400, "Bad SDP", nil)
			tx.Respond(resp)
			return
		}

		localRTPPort := pickFreeUDPPort()
		slog.Info("INVITE accepted",
			"remote_rtp", fmt.Sprintf("%s:%d", remoteIP, remotePort),
			"local_rtp", localRTPPort)

		localIP := getLocalIP()
		sdpAnswer := fmt.Sprintf(
			"v=0\r\no=- 0 0 IN IP4 %s\r\ns=GB28181-Sim\r\nc=IN IP4 %s\r\nt=0 0\r\nm=video %d RTP/AVP 96\r\na=sendonly\r\na=rtpmap:96 PS/90000\r\n",
			localIP, localIP, localRTPPort,
		)

		resp := sip.NewResponseFromRequest(req, 200, "OK", []byte(sdpAnswer))
		resp.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
		tx.Respond(resp)

		callID := ""
		if h := req.CallID(); h != nil {
			callID = h.Value()
		}

		sessCtx, sessCancel := context.WithCancel(ctx)
		sess := &rtpSession{cancel: sessCancel}
		sess.wg.Add(1)

		mu.Lock()
		sessions[callID] = sess
		mu.Unlock()

		go func() {
			defer sess.wg.Done()
			target := &net.UDPAddr{IP: net.ParseIP(remoteIP), Port: remotePort}
			if err := sendRTPStream(sessCtx, target, localRTPPort); err != nil {
				slog.Error("RTP stream stopped", "error", err)
			}
			slog.Info("RTP stream ended", "call_id", callID)
		}()
	})

	srv.OnBye(func(req *sip.Request, tx sip.ServerTransaction) {
		callID := ""
		if h := req.CallID(); h != nil {
			callID = h.Value()
		}
		slog.Info("received BYE", "call_id", callID)

		mu.Lock()
		if sess, ok := sessions[callID]; ok {
			sess.cancel()
			delete(sessions, callID)
		}
		mu.Unlock()

		resp := sip.NewResponseFromRequest(req, 200, "OK", nil)
		tx.Respond(resp)
	})

	listenAddr := fmt.Sprintf("0.0.0.0:%d", *flagLocalPort)
	go func() {
		if err := srv.ListenAndServe(ctx, *flagTransport, listenAddr); err != nil {
			slog.Error("SIP listener stopped", "error", err)
		}
	}()
	slog.Info("SIP listening", "addr", listenAddr, "transport", *flagTransport)

	time.Sleep(200 * time.Millisecond)

	if err := register(ctx, client); err != nil {
		return fmt.Errorf("register: %w", err)
	}

	go keepaliveLoop(ctx, client)

	<-ctx.Done()
	slog.Info("shutting down")

	unregCtx, unregCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer unregCancel()
	unregister(unregCtx, client)

	mu.Lock()
	for _, sess := range sessions {
		sess.cancel()
	}
	mu.Unlock()

	return nil
}



// --- RTP streaming ---

// sendRTPStream launches continuous ffmpeg subprocesses for H.264 video and AAC audio,
// reads frames in real-time, wraps them in PS packs and sends over RTP.
// Unlike pre-generated clips, this produces a linear, non-looping stream.
func sendRTPStream(ctx context.Context, target *net.UDPAddr, localPort int) error {
	conn, err := net.ListenPacket("udp", fmt.Sprintf(":%d", localPort))
	if err != nil {
		return fmt.Errorf("listen UDP :%d: %w", localPort, err)
	}
	defer conn.Close()

	fps := *flagFPS
	hasAudio := !*flagNoAudio

	psMuxer := ps.NewMuxer()
	var mu sync.Mutex
	seq := uint16(0)
	rtpTS := uint32(0)
	ssrc := uint32(0x34020001)
	totalFrames := 0

	sendPS := func(frame *avframe.AVFrame) {
		mu.Lock()
		defer mu.Unlock()

		psData, err := psMuxer.Pack(frame)
		if err != nil {
			return
		}

		const maxPayload = 1400
		for offset := 0; offset < len(psData); offset += maxPayload {
			end := min(offset+maxPayload, len(psData))
			isLast := end >= len(psData)

			pkt := &pionrtp.Packet{
				Header: pionrtp.Header{
					Version:        2,
					PayloadType:    96,
					SequenceNumber: seq,
					Timestamp:      rtpTS,
					SSRC:           ssrc,
					Marker:         isLast,
				},
				Payload: psData[offset:end],
			}
			seq++

			raw, err := pkt.Marshal()
			if err != nil {
				continue
			}
			conn.WriteTo(raw, target)
		}

		if frame.MediaType == avframe.MediaTypeVideo &&
			frame.FrameType != avframe.FrameTypeSequenceHeader {
			totalFrames++
			rtpTS += 90000 / uint32(fps)
			if totalFrames%(fps*5) == 0 {
				slog.Debug("RTP streaming", "frames_sent", totalFrames, "target", target.String())
			}
		}
	}

	slog.Info("RTP streaming started (continuous ffmpeg)",
		"target", target.String(), "fps", fps, "has_audio", hasAudio)

	var wg sync.WaitGroup

	// Start video ffmpeg: outputs raw H.264 Annex-B to stdout
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := streamVideo(ctx, fps, sendPS); err != nil && ctx.Err() == nil {
			slog.Error("video stream error", "error", err)
		}
	}()

	// Start audio ffmpeg: outputs ADTS AAC to stdout
	if hasAudio {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := streamAudio(ctx, sendPS); err != nil && ctx.Err() == nil {
				slog.Error("audio stream error", "error", err)
			}
		}()
	}

	wg.Wait()
	return nil
}

// streamVideo runs ffmpeg producing continuous H.264 Annex-B, parses NAL units,
// and sends each frame through the callback as a PS-ready AVFrame.
func streamVideo(ctx context.Context, fps int, send func(*avframe.AVFrame)) error {
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-re",
		"-f", "lavfi",
		"-i", fmt.Sprintf("testsrc2=size=320x240:rate=%d", fps),
		"-c:v", "libx264",
		"-profile:v", "baseline",
		"-level", "3.0",
		"-x264-params", fmt.Sprintf("keyint=%d:min-keyint=%d:scenecut=0:bframes=0:ref=1", fps, fps),
		"-f", "h264",
		"-",
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()

	// Read Annex-B stream and split into access units (one per frame).
	// An access unit boundary is detected when we see a new SPS (keyframe)
	// or a new slice NAL (type 1 or 5) after accumulating previous NALs.
	buf := make([]byte, 64*1024)
	var residual []byte
	frameCount := int64(0)

	for {
		n, err := stdout.Read(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			// Flush residual
			if len(residual) > 0 {
				emitVideoFrame(residual, frameCount, fps, send)
			}
			return err
		}

		residual = append(residual, buf[:n]...)

		// Split into access units at start code boundaries where a new
		// slice or SPS begins (indicating a new frame).
		for {
			// Find the second start code to delimit one access unit.
			au, rest, found := splitOneAccessUnit(residual)
			if !found {
				break
			}
			emitVideoFrame(au, frameCount, fps, send)
			frameCount++
			residual = rest
		}
	}
}

// splitOneAccessUnit finds a complete access unit in Annex-B data.
// Returns the access unit bytes, remaining bytes, and whether one was found.
// A new access unit boundary is detected when a VCL NAL (slice, type 1 or 5)
// is followed by a non-VCL NAL (SPS/PPS/SEI) or another VCL NAL — indicating
// the start of the next frame.
func splitOneAccessUnit(data []byte) ([]byte, []byte, bool) {
	first := findStartCode(data, 0)
	if first < 0 {
		return nil, data, false
	}

	pos := first
	sawVCL := false // have we seen a VCL (video coding layer) NAL in this AU?

	for {
		scPos := findStartCode(data, pos+3)
		if scPos < 0 {
			return nil, data, false // need more data
		}

		// Determine NAL type at current pos
		scLen := startCodeLen(data, pos)
		nalStart := pos + scLen
		if nalStart < len(data) {
			nalType := data[nalStart] & 0x1F
			if nalType == 1 || nalType == 5 {
				sawVCL = true
			}
		}

		// Check what comes at the next start code
		nextScLen := startCodeLen(data, scPos)
		nextNalStart := scPos + nextScLen
		if nextNalStart >= len(data) {
			return nil, data, false
		}

		nextNalType := data[nextNalStart] & 0x1F
		// A new AU starts when: we already have a VCL NAL, and the next NAL is
		// either a new VCL (1, 5) or a parameter set/SEI that precedes one (7, 8, 6, 9).
		// AUD (type 9) is an explicit AU delimiter.
		if sawVCL && (nextNalType == 9 || nextNalType == 7 || nextNalType == 1 || nextNalType == 5) {
			return data[first:scPos], data[scPos:], true
		}

		pos = scPos
	}
}

// startCodeLen returns the start code length (3 or 4) at the given position.
func startCodeLen(data []byte, pos int) int {
	if pos+3 < len(data) && data[pos] == 0 && data[pos+1] == 0 && data[pos+2] == 0 && data[pos+3] == 1 {
		return 4
	}
	return 3
}

// findStartCode finds the position of the next 00 00 01 or 00 00 00 01 start code.
func findStartCode(data []byte, start int) int {
	for i := start; i+2 < len(data); i++ {
		if data[i] == 0 && data[i+1] == 0 {
			if data[i+2] == 1 {
				return i
			}
			if i+3 < len(data) && data[i+2] == 0 && data[i+3] == 1 {
				return i
			}
		}
	}
	return -1
}

// emitVideoFrame sends a single H.264 access unit (Annex-B) as an AVFrame.
func emitVideoFrame(annexB []byte, frameCount int64, fps int, send func(*avframe.AVFrame)) {
	if len(annexB) == 0 {
		return
	}

	// Detect keyframe by scanning for IDR NAL (type 5)
	isKey := false
	pos := 0
	for {
		sc := findStartCode(annexB, pos)
		if sc < 0 {
			break
		}
		scLen := 3
		if sc+3 < len(annexB) && annexB[sc+2] == 0 {
			scLen = 4
		}
		nalStart := sc + scLen
		if nalStart < len(annexB) {
			nalType := annexB[nalStart] & 0x1F
			if nalType == 5 {
				isKey = true
				break
			}
		}
		pos = sc + scLen
	}

	dts := frameCount * 1000 / int64(fps)
	frameType := avframe.FrameTypeInterframe
	if isKey {
		frameType = avframe.FrameTypeKeyframe
	}

	send(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264,
		frameType, dts, dts, annexB,
	))
}

// streamAudio runs ffmpeg producing continuous ADTS AAC and sends each frame
// through the callback as a PS-ready AVFrame.
func streamAudio(ctx context.Context, send func(*avframe.AVFrame)) error {
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-re",
		"-f", "lavfi",
		"-i", "sine=frequency=440:sample_rate=44100",
		"-c:a", "aac",
		"-b:a", "64k",
		"-ac", "1",
		"-f", "adts",
		"-",
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()

	buf := make([]byte, 8*1024)
	var residual []byte
	frameCount := int64(0)

	for {
		n, err := stdout.Read(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}

		residual = append(residual, buf[:n]...)

		// Parse complete ADTS frames from residual
		for len(residual) >= 7 {
			// ADTS sync word
			if residual[0] != 0xFF || (residual[1]&0xF0) != 0xF0 {
				residual = residual[1:]
				continue
			}

			frameLen := (int(residual[3]&0x03) << 11) |
				(int(residual[4]) << 3) |
				(int(residual[5]) >> 5)
			if frameLen < 7 {
				residual = residual[1:]
				continue
			}
			if len(residual) < frameLen {
				break // need more data
			}

			adtsFrame := make([]byte, frameLen)
			copy(adtsFrame, residual[:frameLen])
			residual = residual[frameLen:]

			dts := frameCount * 1024 * 1000 / 44100 // ~23ms per AAC frame
			send(avframe.NewAVFrame(
				avframe.MediaTypeAudio, avframe.CodecAAC,
				avframe.FrameTypeKeyframe, dts, dts, adtsFrame,
			))
			frameCount++
		}
	}
}

// --- SIP functions ---

func register(ctx context.Context, client *sipgo.Client) error {
	serverIP := extractIP(*flagServerAddr)
	serverPort := parseAddrPort(*flagServerAddr)

	toURI := sip.Uri{User: *flagServerID, Host: serverIP, Port: serverPort}

	req := sip.NewRequest(sip.REGISTER, toURI)
	req.AppendHeader(sip.NewHeader("From", fmt.Sprintf("<sip:%s@%s>;tag=%d", *flagDeviceID, *flagDomain, time.Now().UnixNano()%1000000)))
	req.AppendHeader(sip.NewHeader("To", fmt.Sprintf("<sip:%s@%s>", *flagDeviceID, *flagDomain)))
	req.AppendHeader(sip.NewHeader("Expires", "3600"))
	req.AppendHeader(sip.NewHeader("Contact", fmt.Sprintf("<sip:%s@%s:%d>", *flagDeviceID, getLocalIP(), *flagLocalPort)))

	tx, err := client.TransactionRequest(ctx, req)
	if err != nil {
		return fmt.Errorf("send REGISTER: %w", err)
	}
	defer tx.Terminate()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case resp := <-tx.Responses():
		if resp.StatusCode == 200 {
			slog.Info("registered successfully", "device_id", *flagDeviceID)
			return nil
		}
		return fmt.Errorf("REGISTER rejected: %d %s", resp.StatusCode, resp.Reason)
	case <-time.After(5 * time.Second):
		return fmt.Errorf("REGISTER timeout")
	}
}

func unregister(ctx context.Context, client *sipgo.Client) {
	serverIP := extractIP(*flagServerAddr)
	serverPort := parseAddrPort(*flagServerAddr)

	toURI := sip.Uri{User: *flagServerID, Host: serverIP, Port: serverPort}

	req := sip.NewRequest(sip.REGISTER, toURI)
	req.AppendHeader(sip.NewHeader("From", fmt.Sprintf("<sip:%s@%s>;tag=%d", *flagDeviceID, *flagDomain, time.Now().UnixNano()%1000000)))
	req.AppendHeader(sip.NewHeader("To", fmt.Sprintf("<sip:%s@%s>", *flagDeviceID, *flagDomain)))
	req.AppendHeader(sip.NewHeader("Expires", "0"))

	tx, err := client.TransactionRequest(ctx, req)
	if err != nil {
		slog.Warn("unregister failed", "error", err)
		return
	}
	defer tx.Terminate()

	select {
	case resp := <-tx.Responses():
		slog.Info("unregistered", "status", resp.StatusCode)
	case <-time.After(2 * time.Second):
		slog.Warn("unregister timeout")
	}
}

func keepaliveLoop(ctx context.Context, client *sipgo.Client) {
	sn := 1
	ticker := time.NewTicker(*flagKeepalive)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := sendKeepalive(ctx, client, sn); err != nil {
				slog.Warn("keepalive failed", "error", err)
			} else {
				slog.Debug("keepalive sent", "sn", sn)
			}
			sn++
		}
	}
}

func sendKeepalive(ctx context.Context, client *sipgo.Client, sn int) error {
	body := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<Notify>
  <CmdType>Keepalive</CmdType>
  <SN>%d</SN>
  <DeviceID>%s</DeviceID>
  <Status>OK</Status>
</Notify>`, sn, *flagDeviceID)

	serverIP := extractIP(*flagServerAddr)
	serverPort := parseAddrPort(*flagServerAddr)

	toURI := sip.Uri{User: *flagServerID, Host: serverIP, Port: serverPort}

	req := sip.NewRequest(sip.MESSAGE, toURI)
	req.AppendHeader(sip.NewHeader("From", fmt.Sprintf("<sip:%s@%s>;tag=%d", *flagDeviceID, *flagDomain, time.Now().UnixNano()%1000000)))
	req.AppendHeader(sip.NewHeader("To", fmt.Sprintf("<sip:%s@%s>", *flagServerID, serverIP)))
	req.AppendHeader(sip.NewHeader("Content-Type", "Application/MANSCDP+xml"))
	req.SetBody([]byte(body))

	tx, err := client.TransactionRequest(ctx, req)
	if err != nil {
		return err
	}
	defer tx.Terminate()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case resp := <-tx.Responses():
		if resp.StatusCode >= 300 {
			return fmt.Errorf("keepalive rejected: %d", resp.StatusCode)
		}
		return nil
	case <-time.After(3 * time.Second):
		return fmt.Errorf("keepalive timeout")
	}
}

func sendMessage(ctx context.Context, client *sipgo.Client, body []byte) {
	serverIP := extractIP(*flagServerAddr)
	serverPort := parseAddrPort(*flagServerAddr)

	toURI := sip.Uri{User: *flagServerID, Host: serverIP, Port: serverPort}

	req := sip.NewRequest(sip.MESSAGE, toURI)
	req.AppendHeader(sip.NewHeader("From", fmt.Sprintf("<sip:%s@%s>;tag=%d", *flagDeviceID, *flagDomain, time.Now().UnixNano()%1000000)))
	req.AppendHeader(sip.NewHeader("To", fmt.Sprintf("<sip:%s@%s>", *flagServerID, serverIP)))
	req.AppendHeader(sip.NewHeader("Content-Type", "Application/MANSCDP+xml"))
	req.SetBody(body)

	tx, err := client.TransactionRequest(ctx, req)
	if err != nil {
		slog.Warn("send message failed", "error", err)
		return
	}
	defer tx.Terminate()

	select {
	case resp := <-tx.Responses():
		slog.Debug("message response", "status", resp.StatusCode)
	case <-time.After(3 * time.Second):
		slog.Warn("message timeout")
	}
}

// --- XML helpers ---

func buildCatalogResponse(queryBody []byte) []byte {
	type query struct {
		SN       int    `xml:"SN"`
		DeviceID string `xml:"DeviceID"`
	}
	var q query
	xml.Unmarshal(queryBody, &q)

	type item struct {
		DeviceID     string  `xml:"DeviceID"`
		Name         string  `xml:"Name"`
		Manufacturer string  `xml:"Manufacturer"`
		Status       string  `xml:"Status"`
		PTZType      int     `xml:"PTZType"`
		Longitude    float64 `xml:"Longitude"`
		Latitude     float64 `xml:"Latitude"`
	}

	type deviceList struct {
		Num   int    `xml:"Num,attr"`
		Items []item `xml:"Item"`
	}

	type catalogResp struct {
		XMLName    xml.Name   `xml:"Response"`
		CmdType    string     `xml:"CmdType"`
		SN         int        `xml:"SN"`
		DeviceID   string     `xml:"DeviceID"`
		SumNum     int        `xml:"SumNum"`
		DeviceList deviceList `xml:"DeviceList"`
	}

	items := make([]item, len(channels))
	for i, ch := range channels {
		items[i] = item{
			DeviceID:     ch.ID,
			Name:         ch.Name,
			Manufacturer: ch.Manufacturer,
			Status:       ch.Status,
			PTZType:      ch.PTZType,
			Longitude:    ch.Longitude,
			Latitude:     ch.Latitude,
		}
	}

	resp := catalogResp{
		CmdType:  "Catalog",
		SN:       q.SN,
		DeviceID: *flagDeviceID,
		SumNum:   len(channels),
		DeviceList: deviceList{
			Num:   len(channels),
			Items: items,
		},
	}

	data, _ := xml.MarshalIndent(resp, "", "  ")
	return append([]byte(xml.Header), data...)
}

// --- General helpers ---

func parseCmdType(body []byte) string {
	type msg struct {
		CmdType string `xml:"CmdType"`
	}
	var m msg
	xml.Unmarshal(body, &m)
	return m.CmdType
}

func parseSDPPort(sdp string) int {
	for line := range strings.SplitSeq(sdp, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "m=") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				port := 0
				fmt.Sscanf(parts[1], "%d", &port)
				return port
			}
		}
	}
	return 0
}

func extractIP(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}

func parseAddrPort(addr string) int {
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return 5060
	}
	port := 0
	fmt.Sscanf(portStr, "%d", &port)
	if port == 0 {
		return 5060
	}
	return port
}

func getLocalIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "127.0.0.1"
	}
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() != nil {
				return ipnet.IP.String()
			}
		}
	}
	return "127.0.0.1"
}

func pickFreeUDPPort() int {
	conn, err := net.ListenPacket("udp", ":0")
	if err != nil {
		return 30000
	}
	port := conn.LocalAddr().(*net.UDPAddr).Port
	conn.Close()
	return port
}
