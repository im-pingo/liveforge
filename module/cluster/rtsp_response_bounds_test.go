package cluster

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestRTSPResponseRejectsUnboundedOrAmbiguousInput(t *testing.T) {
	for name, raw := range map[string]string{
		"status line":      "RTSP/1.0 200 " + strings.Repeat("x", 8192) + "\r\n\r\n",
		"header line":      "RTSP/1.0 200 OK\r\nX-Test: " + strings.Repeat("x", 8192) + "\r\n\r\n",
		"header total":     "RTSP/1.0 200 OK\r\n" + strings.Repeat("X-Test: "+strings.Repeat("x", 1000)+"\r\n", 66) + "\r\n",
		"negative length":  "RTSP/1.0 200 OK\r\nContent-Length: -1\r\n\r\n",
		"invalid length":   "RTSP/1.0 200 OK\r\nContent-Length: nonsense\r\n\r\n",
		"duplicate length": "RTSP/1.0 200 OK\r\nContent-Length: 0\r\nContent-Length: 1\r\n\r\nx",
		"malformed header": "RTSP/1.0 200 OK\r\ninvalid\r\n\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			rc := &rtspClient{br: bufio.NewReader(strings.NewReader(raw))}
			if _, err := rc.readResponse(); err == nil {
				t.Fatal("accepted unsafe RTSP response framing")
			}
		})
	}
}

type rtspResponseBodyProbe struct{ read bool }

var errRTSPBodyProbe = errors.New("response body was read before rejecting length")

func (p *rtspResponseBodyProbe) Read([]byte) (int, error) {
	p.read = true
	return 0, errRTSPBodyProbe
}

func TestRTSPResponseRejectsOversizedBodyBeforeReading(t *testing.T) {
	probe := &rtspResponseBodyProbe{}
	raw := "RTSP/1.0 200 OK\r\nContent-Length: 1048577\r\n\r\n"
	rc := &rtspClient{br: bufio.NewReader(io.MultiReader(strings.NewReader(raw), probe))}
	_, err := rc.readResponse()
	if err == nil || probe.read || errors.Is(err, errRTSPBodyProbe) {
		t.Fatalf("oversized response was not rejected at its header: read=%v err=%v", probe.read, err)
	}
}
