package rtsp

import (
	"bufio"
	"bytes"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestReadRequest(t *testing.T) {
	raw := "DESCRIBE rtsp://host/live/test RTSP/1.0\r\n" +
		"CSeq: 1\r\n" +
		"Accept: application/sdp\r\n" +
		"\r\n"
	r := bufio.NewReader(bytes.NewReader([]byte(raw)))
	req, err := ReadRequest(r)
	if err != nil {
		t.Fatalf("ReadRequest: %v", err)
	}
	if req.Method != "DESCRIBE" {
		t.Errorf("Method = %q", req.Method)
	}
	if req.URL != "rtsp://host/live/test" {
		t.Errorf("URL = %q", req.URL)
	}
	if req.Headers.Get("CSeq") != "1" {
		t.Errorf("CSeq = %q", req.Headers.Get("CSeq"))
	}
}

func TestReadRequestWithBody(t *testing.T) {
	body := "v=0\r\no=- 0 0 IN IP4 0.0.0.0\r\ns=test\r\nt=0 0\r\n"
	raw := "ANNOUNCE rtsp://host/live/test RTSP/1.0\r\n" +
		"CSeq: 2\r\n" +
		"Content-Type: application/sdp\r\n" +
		"Content-Length: " + fmt.Sprintf("%d", len(body)) + "\r\n" +
		"\r\n" + body
	r := bufio.NewReader(bytes.NewReader([]byte(raw)))
	req, err := ReadRequest(r)
	if err != nil {
		t.Fatalf("ReadRequest: %v", err)
	}
	if req.Method != "ANNOUNCE" {
		t.Errorf("Method = %q", req.Method)
	}
	if string(req.Body) != body {
		t.Errorf("Body = %q", string(req.Body))
	}
}

func TestWriteResponse(t *testing.T) {
	var buf bytes.Buffer
	resp := &Response{
		StatusCode: 200,
		Reason:     "OK",
		Headers:    make(http.Header),
	}
	resp.Headers.Set("CSeq", "1")
	resp.Headers.Set("Session", "abc123")
	err := WriteResponse(&buf, resp)
	if err != nil {
		t.Fatalf("WriteResponse: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "RTSP/1.0 200 OK\r\n") {
		t.Errorf("missing status line in: %q", out)
	}
	if !strings.Contains(strings.ToLower(out), "cseq: 1") {
		t.Errorf("missing CSeq in: %q", out)
	}
}

func TestReadRequestInvalid(t *testing.T) {
	raw := "INVALID\r\n\r\n"
	r := bufio.NewReader(bytes.NewReader([]byte(raw)))
	_, err := ReadRequest(r)
	if err == nil {
		t.Fatal("expected error for invalid request line")
	}
}

func TestWriteResponseWithBody(t *testing.T) {
	var buf bytes.Buffer
	body := []byte("v=0\r\n")
	resp := &Response{
		StatusCode: 200,
		Reason:     "OK",
		Headers:    make(http.Header),
		Body:       body,
	}
	resp.Headers.Set("CSeq", "1")
	resp.Headers.Set("Content-Length", fmt.Sprintf("%d", len(body)))
	err := WriteResponse(&buf, resp)
	if err != nil {
		t.Fatalf("WriteResponse: %v", err)
	}
	if !strings.Contains(buf.String(), "v=0\r\n") {
		t.Error("missing body in response")
	}
}
