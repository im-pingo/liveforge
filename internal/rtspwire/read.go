// Package rtspwire bounds RTSP control-message framing shared by servers and peers.
package rtspwire

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

const (
	maxLineBytes   = 8 << 10
	maxHeaderBytes = 64 << 10
	maxBodyBytes   = 1 << 20
)

// ReadHead reads the start line and headers with 8 KiB line and 64 KiB total
// wire-byte limits, including line terminators. It leaves the body unread.
func ReadHead(r *bufio.Reader) (string, http.Header, error) {
	line, used, err := readLine(r, maxLineBytes)
	if err != nil {
		return "", nil, fmt.Errorf("RTSP start line: %w", err)
	}
	headers := make(http.Header)
	for {
		limit := min(maxLineBytes, maxHeaderBytes-used)
		hline, size, err := readLine(r, limit)
		if err != nil {
			return "", nil, fmt.Errorf("RTSP headers: %w", err)
		}
		used += size
		if hline == "" {
			return line, headers, nil
		}
		key, value, ok := strings.Cut(hline, ":")
		if !ok || !headerToken(key) || !headerValue(value) {
			return "", nil, errors.New("malformed RTSP header")
		}
		headers.Add(key, strings.Trim(value, " \t"))
	}
}

func readLine(r *bufio.Reader, limit int) (string, int, error) {
	var line []byte
	for {
		if len(line) >= limit {
			return "", 0, errors.New("RTSP line or header section exceeds limit")
		}
		part, err := r.ReadSlice('\n')
		if len(part) > limit-len(line) {
			return "", 0, errors.New("RTSP line or header section exceeds limit")
		}
		line = append(line, part...)
		if err == nil {
			value := strings.TrimSuffix(strings.TrimSuffix(string(line), "\n"), "\r")
			return value, len(line), nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return "", 0, err
		}
	}
}

func headerToken(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			strings.ContainsRune("!#$%&'*+-.^_`|~", c) {
			continue
		}
		return false
	}
	return true
}

func headerValue(s string) bool {
	for i := range len(s) {
		if s[i] < 32 && s[i] != '\t' || s[i] == 127 {
			return false
		}
	}
	return true
}

// ReadBody accepts one unsigned decimal Content-Length of at most 1 MiB.
// Ambiguous/unsupported framing is rejected before allocation or body reads.
func ReadBody(r *bufio.Reader, headers http.Header) ([]byte, error) {
	if _, present := headers["Transfer-Encoding"]; present {
		return nil, errors.New("RTSP Transfer-Encoding is unsupported")
	}
	values, present := headers["Content-Length"]
	if !present {
		return nil, nil
	}
	if len(values) != 1 || values[0] == "" {
		return nil, errors.New("RTSP requires one nonempty Content-Length")
	}
	for _, c := range values[0] {
		if c < '0' || c > '9' {
			return nil, errors.New("RTSP Content-Length must be unsigned decimal")
		}
	}
	n, err := strconv.ParseUint(values[0], 10, 64)
	if err != nil || n > maxBodyBytes {
		return nil, errors.New("RTSP Content-Length exceeds 1 MiB")
	}
	if n == 0 {
		return nil, nil
	}
	body := make([]byte, int(n))
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, fmt.Errorf("RTSP body: %w", err)
	}
	return body, nil
}
