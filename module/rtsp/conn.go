package rtsp

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/im-pingo/liveforge/internal/rtspwire"
)

// Request represents an RTSP request.
type Request struct {
	Method  string
	URL     string
	Proto   string
	Headers http.Header
	Body    []byte
}

// Response represents an RTSP response.
type Response struct {
	StatusCode int
	Reason     string
	Headers    http.Header
	Body       []byte
}

// ReadRequest reads an RTSP request from a buffered reader.
func ReadRequest(r *bufio.Reader) (*Request, error) {
	// Read request line: "METHOD URL RTSP/1.0\r\n"
	line, headers, err := rtspwire.ReadHead(r)
	if err != nil {
		return nil, fmt.Errorf("reading request line: %w", err)
	}

	parts := strings.SplitN(line, " ", 3)
	if len(parts) != 3 {
		return nil, fmt.Errorf("malformed request line: %q", line)
	}

	req := &Request{
		Method:  parts[0],
		URL:     parts[1],
		Proto:   parts[2],
		Headers: headers,
	}
	req.Body, err = rtspwire.ReadBody(r, req.Headers)
	if err != nil {
		return nil, err
	}

	return req, nil
}

// WriteResponse writes an RTSP response to a writer.
func WriteResponse(w io.Writer, resp *Response) error {
	// Write status line
	if _, err := fmt.Fprintf(w, "RTSP/1.0 %d %s\r\n", resp.StatusCode, resp.Reason); err != nil {
		return err
	}

	// Write headers
	for key, vals := range resp.Headers {
		for _, v := range vals {
			if _, err := fmt.Fprintf(w, "%s: %s\r\n", key, v); err != nil {
				return err
			}
		}
	}

	// Write blank line separating headers from body
	if _, err := fmt.Fprint(w, "\r\n"); err != nil {
		return err
	}

	// Write body if present
	if len(resp.Body) > 0 {
		if _, err := w.Write(resp.Body); err != nil {
			return err
		}
	}

	return nil
}
