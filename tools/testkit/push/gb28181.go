package push

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/muxer/ps"
	"github.com/im-pingo/liveforge/tools/testkit/report"
	"github.com/im-pingo/liveforge/tools/testkit/source"
	pionrtp "github.com/pion/rtp/v2"
)

const (
	gb28181DeviceID  = "34020000001110000001"
	gb28181Domain    = "3402000000"
	gb28181ServerID  = "34020000002000000001"
	gb28181MaxRTPPay = 1400
	gb28181FPS       = 25
)

// inviteInfo carries the SDP negotiation result from the INVITE handler to
// the streaming goroutine.
type inviteInfo struct {
	remoteIP   string
	remotePort int
	localPort  int
}

// gb28181Pusher implements Pusher for the GB28181 video surveillance protocol.
// It simulates a GB28181 camera device: registers via SIP, handles INVITE from
// the server, and streams RTP/PS packets.
type gb28181Pusher struct{}

// Push performs a full GB28181 device simulation: SIP REGISTER, wait for
// INVITE (triggered via the API), and stream RTP/PS media from the source.
//
// cfg.Target is the SIP server address (e.g. "127.0.0.1:5060").
// cfg.Token is the API server address (e.g. "127.0.0.1:8080") used to trigger
// the INVITE via POST /api/v1/gb28181/channels/{channelID}/play.
func (p *gb28181Pusher) Push(ctx context.Context, src source.Source, cfg PushConfig) (*report.PushReport, error) {
	start := time.Now()
	var framesSent int64
	var bytesSent int64

	serverAddr := cfg.Target

	// --- SIP User Agent ---
	ua, err := sipgo.NewUA(
		sipgo.WithUserAgent("GB28181-Test"),
		sipgo.WithUserAgentHostname(gb28181Domain),
	)
	if err != nil {
		return buildPushReport(cfg, start, framesSent, bytesSent),
			fmt.Errorf("gb28181: create UA: %w", err)
	}

	srv, err := sipgo.NewServer(ua)
	if err != nil {
		return buildPushReport(cfg, start, framesSent, bytesSent),
			fmt.Errorf("gb28181: create SIP server: %w", err)
	}

	client, err := sipgo.NewClient(ua)
	if err != nil {
		return buildPushReport(cfg, start, framesSent, bytesSent),
			fmt.Errorf("gb28181: create SIP client: %w", err)
	}

	// --- INVITE handler ---
	inviteReady := make(chan inviteInfo, 1)

	srv.OnInvite(func(req *sip.Request, tx sip.ServerTransaction) {
		sdpBody := string(req.Body())
		remotePort := gb28181ParseSDPPort(sdpBody)
		remoteIP := gb28181ExtractIP(serverAddr)
		if remotePort == 0 {
			resp := sip.NewResponseFromRequest(req, 400, "Bad SDP", nil)
			tx.Respond(resp) //nolint:errcheck
			return
		}

		localRTPPort := gb28181PickFreeUDPPort()
		localIP := "127.0.0.1"

		sdpAnswer := fmt.Sprintf(
			"v=0\r\no=- 0 0 IN IP4 %s\r\ns=GB28181-Test\r\nc=IN IP4 %s\r\nt=0 0\r\nm=video %d RTP/AVP 96\r\na=sendonly\r\na=rtpmap:96 PS/90000\r\n",
			localIP, localIP, localRTPPort,
		)

		resp := sip.NewResponseFromRequest(req, 200, "OK", []byte(sdpAnswer))
		resp.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
		tx.Respond(resp) //nolint:errcheck

		inviteReady <- inviteInfo{
			remoteIP:   remoteIP,
			remotePort: remotePort,
			localPort:  localRTPPort,
		}
	})

	// --- BYE handler ---
	srv.OnBye(func(req *sip.Request, tx sip.ServerTransaction) {
		resp := sip.NewResponseFromRequest(req, 200, "OK", nil)
		tx.Respond(resp) //nolint:errcheck
	})

	// --- MESSAGE handler (catalog queries) ---
	srv.OnMessage(func(req *sip.Request, tx sip.ServerTransaction) {
		body := req.Body()
		if gb28181ParseCmdType(body) == "Catalog" {
			catalogResp := gb28181BuildCatalogResponse(body, gb28181DeviceID)
			gb28181SendMessage(ctx, client, catalogResp, serverAddr)
		}
		resp := sip.NewResponseFromRequest(req, 200, "OK", nil)
		tx.Respond(resp) //nolint:errcheck
	})

	// --- Start SIP listener ---
	localSIPPort := gb28181PickFreeUDPPort()
	listenAddr := fmt.Sprintf("127.0.0.1:%d", localSIPPort)
	go srv.ListenAndServe(ctx, "udp", listenAddr) //nolint:errcheck
	time.Sleep(100 * time.Millisecond)

	// --- SIP REGISTER ---
	if err := gb28181Register(ctx, client, serverAddr, localSIPPort); err != nil {
		return buildPushReport(cfg, start, framesSent, bytesSent),
			fmt.Errorf("gb28181: register: %w", err)
	}

	// --- Send initial keepalive ---
	if err := gb28181SendKeepalive(ctx, client, serverAddr, 1); err != nil {
		return buildPushReport(cfg, start, framesSent, bytesSent),
			fmt.Errorf("gb28181: keepalive: %w", err)
	}

	// --- Send catalog response so server registers our channel ---
	catalogBody := gb28181BuildCatalogResponse(
		[]byte(fmt.Sprintf(`<Query><CmdType>Catalog</CmdType><SN>1</SN><DeviceID>%s</DeviceID></Query>`, gb28181DeviceID)),
		gb28181DeviceID,
	)
	gb28181SendMessage(ctx, client, catalogBody, serverAddr)
	// Small delay to let server process the catalog.
	time.Sleep(100 * time.Millisecond)

	// --- Trigger INVITE via API ---
	apiAddr := cfg.Token
	if apiAddr != "" {
		triggerURL := fmt.Sprintf("http://%s/api/v1/gb28181/channels/%s/play", apiAddr, gb28181DeviceID)
		triggerReq, err := http.NewRequestWithContext(ctx, http.MethodPost, triggerURL, nil)
		if err != nil {
			return buildPushReport(cfg, start, framesSent, bytesSent),
				fmt.Errorf("gb28181: build trigger request: %w", err)
		}
		resp, err := http.DefaultClient.Do(triggerReq)
		if err != nil {
			return buildPushReport(cfg, start, framesSent, bytesSent),
				fmt.Errorf("gb28181: trigger INVITE via API: %w", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return buildPushReport(cfg, start, framesSent, bytesSent),
				fmt.Errorf("gb28181: trigger API returned %d", resp.StatusCode)
		}
	}

	// --- Wait for INVITE ---
	var info inviteInfo
	select {
	case info = <-inviteReady:
	case <-ctx.Done():
		return buildPushReport(cfg, start, framesSent, bytesSent), ctx.Err()
	case <-time.After(10 * time.Second):
		return buildPushReport(cfg, start, framesSent, bytesSent),
			fmt.Errorf("gb28181: INVITE timeout")
	}

	// --- Stream RTP/PS ---
	conn, err := net.ListenPacket("udp", fmt.Sprintf("127.0.0.1:%d", info.localPort))
	if err != nil {
		return buildPushReport(cfg, start, framesSent, bytesSent),
			fmt.Errorf("gb28181: listen UDP %d: %w", info.localPort, err)
	}
	defer conn.Close()

	target := &net.UDPAddr{IP: net.ParseIP(info.remoteIP), Port: info.remotePort}
	psMuxer := ps.NewMuxer()
	var mu sync.Mutex
	seq := uint16(0)
	rtpTS := uint32(0)
	ssrc := uint32(0x34020001)

	var deadline time.Time
	if cfg.Duration > 0 {
		deadline = start.Add(cfg.Duration)
	}

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
				fmt.Errorf("gb28181: read source frame: %w", err)
		}

		psData, err := psMuxer.Pack(frame)
		if err != nil {
			continue
		}

		mu.Lock()
		for offset := 0; offset < len(psData); offset += gb28181MaxRTPPay {
			end := offset + gb28181MaxRTPPay
			if end > len(psData) {
				end = len(psData)
			}
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
			if _, err := conn.WriteTo(raw, target); err != nil {
				mu.Unlock()
				return buildPushReport(cfg, start, framesSent, bytesSent),
					fmt.Errorf("gb28181: write RTP: %w", err)
			}
			bytesSent += int64(len(raw))
		}
		mu.Unlock()

		if frame.MediaType == avframe.MediaTypeVideo &&
			frame.FrameType != avframe.FrameTypeSequenceHeader {
			rtpTS += 90000 / uint32(gb28181FPS)
		}

		framesSent++
	}

	return buildPushReport(cfg, start, framesSent, bytesSent), nil
}

// --- SIP helpers ---

func gb28181Register(ctx context.Context, client *sipgo.Client, serverAddr string, localSIPPort int) error {
	serverIP := gb28181ExtractIP(serverAddr)
	serverPort := gb28181ParseAddrPort(serverAddr)

	toURI := sip.Uri{User: gb28181ServerID, Host: serverIP, Port: serverPort}

	req := sip.NewRequest(sip.REGISTER, toURI)
	req.AppendHeader(sip.NewHeader("From", fmt.Sprintf("<sip:%s@%s>;tag=%d", gb28181DeviceID, gb28181Domain, time.Now().UnixNano()%1000000)))
	req.AppendHeader(sip.NewHeader("To", fmt.Sprintf("<sip:%s@%s>", gb28181DeviceID, gb28181Domain)))
	req.AppendHeader(sip.NewHeader("Expires", "3600"))
	req.AppendHeader(sip.NewHeader("Contact", fmt.Sprintf("<sip:%s@127.0.0.1:%d>", gb28181DeviceID, localSIPPort)))

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
			return nil
		}
		return fmt.Errorf("REGISTER rejected: %d %s", resp.StatusCode, resp.Reason)
	case <-time.After(5 * time.Second):
		return fmt.Errorf("REGISTER timeout")
	}
}

func gb28181SendKeepalive(ctx context.Context, client *sipgo.Client, serverAddr string, sn int) error {
	body := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<Notify>
  <CmdType>Keepalive</CmdType>
  <SN>%d</SN>
  <DeviceID>%s</DeviceID>
  <Status>OK</Status>
</Notify>`, sn, gb28181DeviceID)

	serverIP := gb28181ExtractIP(serverAddr)
	serverPort := gb28181ParseAddrPort(serverAddr)

	toURI := sip.Uri{User: gb28181ServerID, Host: serverIP, Port: serverPort}

	req := sip.NewRequest(sip.MESSAGE, toURI)
	req.AppendHeader(sip.NewHeader("From", fmt.Sprintf("<sip:%s@%s>;tag=%d", gb28181DeviceID, gb28181Domain, time.Now().UnixNano()%1000000)))
	req.AppendHeader(sip.NewHeader("To", fmt.Sprintf("<sip:%s@%s>", gb28181ServerID, serverIP)))
	req.AppendHeader(sip.NewHeader("Content-Type", "Application/MANSCDP+xml"))
	req.SetBody([]byte(body))

	tx, err := client.TransactionRequest(ctx, req)
	if err != nil {
		return fmt.Errorf("send keepalive: %w", err)
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

func gb28181SendMessage(ctx context.Context, client *sipgo.Client, body []byte, serverAddr string) {
	serverIP := gb28181ExtractIP(serverAddr)
	serverPort := gb28181ParseAddrPort(serverAddr)

	toURI := sip.Uri{User: gb28181ServerID, Host: serverIP, Port: serverPort}

	req := sip.NewRequest(sip.MESSAGE, toURI)
	req.AppendHeader(sip.NewHeader("From", fmt.Sprintf("<sip:%s@%s>;tag=%d", gb28181DeviceID, gb28181Domain, time.Now().UnixNano()%1000000)))
	req.AppendHeader(sip.NewHeader("To", fmt.Sprintf("<sip:%s@%s>", gb28181ServerID, serverIP)))
	req.AppendHeader(sip.NewHeader("Content-Type", "Application/MANSCDP+xml"))
	req.SetBody(body)

	tx, err := client.TransactionRequest(ctx, req)
	if err != nil {
		return
	}
	defer tx.Terminate()

	select {
	case <-tx.Responses():
	case <-time.After(3 * time.Second):
	}
}

// --- XML helpers ---

func gb28181ParseCmdType(body []byte) string {
	type msg struct {
		CmdType string `xml:"CmdType"`
	}
	var m msg
	xml.Unmarshal(body, &m) //nolint:errcheck
	return m.CmdType
}

func gb28181BuildCatalogResponse(queryBody []byte, deviceID string) []byte {
	type query struct {
		SN int `xml:"SN"`
	}
	var q query
	xml.Unmarshal(queryBody, &q) //nolint:errcheck

	return []byte(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<Response>
  <CmdType>Catalog</CmdType>
  <SN>%d</SN>
  <DeviceID>%s</DeviceID>
  <SumNum>1</SumNum>
  <DeviceList Num="1">
    <Item>
      <DeviceID>%s</DeviceID>
      <Name>Test Camera</Name>
      <Manufacturer>TestKit</Manufacturer>
      <Status>ON</Status>
    </Item>
  </DeviceList>
</Response>`, q.SN, deviceID, deviceID))
}

// --- Network helpers ---

func gb28181ParseSDPPort(sdp string) int {
	for _, line := range strings.Split(sdp, "\n") {
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

func gb28181ExtractIP(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}

func gb28181ParseAddrPort(addr string) int {
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

func gb28181PickFreeUDPPort() int {
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		return 30000
	}
	port := conn.LocalAddr().(*net.UDPAddr).Port
	conn.Close()
	return port
}
