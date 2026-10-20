package rtsp

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestReadRequestRejectsUnboundedOrAmbiguousInput(t *testing.T) {
	for name, raw := range map[string]string{
		"request line":      "OPTIONS rtsp://host/" + strings.Repeat("x", 8192) + " RTSP/1.0\r\n\r\n",
		"header line":       "OPTIONS * RTSP/1.0\r\nX-Test: " + strings.Repeat("x", 8192) + "\r\n\r\n",
		"header total":      "OPTIONS * RTSP/1.0\r\n" + strings.Repeat("X-Test: "+strings.Repeat("x", 1000)+"\r\n", 66) + "\r\n",
		"negative length":   "ANNOUNCE * RTSP/1.0\r\nContent-Length: -1\r\n\r\n",
		"signed length":     "ANNOUNCE * RTSP/1.0\r\nContent-Length: +1\r\n\r\nx",
		"duplicate length":  "ANNOUNCE * RTSP/1.0\r\nContent-Length: 0\r\nContent-Length: 1\r\n\r\nx",
		"empty length":      "ANNOUNCE * RTSP/1.0\r\nContent-Length:\r\n\r\n",
		"transfer encoding": "ANNOUNCE * RTSP/1.0\r\nTransfer-Encoding: chunked\r\n\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ReadRequest(bufio.NewReader(strings.NewReader(raw))); err == nil {
				t.Fatal("accepted unsafe RTSP framing")
			}
		})
	}
}

type rejectedBodyProbe struct{ read bool }

var errBodyProbe = errors.New("body was read before rejecting length")

func (p *rejectedBodyProbe) Read([]byte) (int, error) {
	p.read = true
	return 0, errBodyProbe
}

func TestReadRequestRejectsOversizedBodyBeforeReading(t *testing.T) {
	probe := &rejectedBodyProbe{}
	raw := "ANNOUNCE * RTSP/1.0\r\nContent-Length: 1048577\r\n\r\n"
	_, err := ReadRequest(bufio.NewReader(io.MultiReader(strings.NewReader(raw), probe)))
	if err == nil || probe.read || errors.Is(err, errBodyProbe) {
		t.Fatalf("oversized body was not rejected at its header: read=%v err=%v", probe.read, err)
	}
}

func TestReadRequestPreservesBodyAndFollowingMessage(t *testing.T) {
	body := strings.Repeat("x", 1<<20)
	reader := bufio.NewReader(strings.NewReader("ANNOUNCE * RTSP/1.0\r\nContent-Length: 1048576\r\n\r\n" + body + "OPTIONS * RTSP/1.0\r\nCSeq: 2\r\n\r\n"))
	first, err := ReadRequest(reader)
	if err != nil || string(first.Body) != body {
		t.Fatalf("valid maximum body: %v", err)
	}
	second, err := ReadRequest(reader)
	if err != nil || second.Method != "OPTIONS" || second.Headers.Get("CSeq") != "2" {
		t.Fatalf("following request was consumed: %+v %v", second, err)
	}
}
