package sip

import (
	"crypto/md5"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/emiago/sipgo/sip"
)

// DigestAuth provides SIP digest authentication helpers.
type DigestAuth struct {
	realm    string
	password string
	ttl      time.Duration
	now      func() time.Time
	mu       sync.Mutex
	nonces   map[string]time.Time
}

// NewDigestAuth creates a new digest auth helper.
func NewDigestAuth(realm, password string) *DigestAuth {
	return newDigestAuthWithClock(realm, password, 2*time.Minute, time.Now)
}

func newDigestAuthWithClock(realm, password string, ttl time.Duration, now func() time.Time) *DigestAuth {
	return &DigestAuth{
		realm: realm, password: password, ttl: ttl, now: now,
		nonces: make(map[string]time.Time),
	}
}

// Challenge creates a 401 Unauthorized response with a WWW-Authenticate header.
func (d *DigestAuth) Challenge(req *sip.Request) *sip.Response {
	nonce := generateNonce()
	now := d.now()
	d.mu.Lock()
	for value, expiresAt := range d.nonces {
		if !expiresAt.After(now) {
			delete(d.nonces, value)
		}
	}
	d.nonces[nonce] = now.Add(d.ttl)
	d.mu.Unlock()
	resp := sip.NewResponseFromRequest(req, 401, "Unauthorized", nil)
	wwwAuth := fmt.Sprintf(`Digest realm="%s", nonce="%s", algorithm=MD5`, d.realm, nonce)
	resp.AppendHeader(sip.NewHeader("WWW-Authenticate", wwwAuth))
	return resp
}

// Verify checks the Authorization header against the expected credentials.
func (d *DigestAuth) Verify(req *sip.Request) bool {
	authHeader := req.GetHeader("Authorization")
	if authHeader == nil {
		return false
	}

	params := parseDigestParams(authHeader.Value())
	username := params["username"]
	nonce := params["nonce"]
	uri := params["uri"]
	responseHash := params["response"]

	if username == "" || nonce == "" || uri == "" || responseHash == "" || params["realm"] != d.realm {
		return false
	}

	// Compute expected response: MD5(HA1:nonce:HA2)
	// HA1 = MD5(username:realm:password)
	// HA2 = MD5(method:uri)
	ha1 := md5Hex(username + ":" + d.realm + ":" + d.password)
	ha2 := md5Hex(string(req.Method) + ":" + uri)
	expected := md5Hex(ha1 + ":" + nonce + ":" + ha2)

	d.mu.Lock()
	defer d.mu.Unlock()
	expiresAt, issued := d.nonces[nonce]
	if !issued || !expiresAt.After(d.now()) {
		delete(d.nonces, nonce)
		return false
	}
	if subtle.ConstantTimeCompare([]byte(responseHash), []byte(expected)) != 1 {
		return false
	}
	delete(d.nonces, nonce)
	return true
}

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func generateNonce() string {
	b := make([]byte, 16)
	rand.Read(b)
	ts := time.Now().UnixNano()
	return fmt.Sprintf("%x%016x", b, ts)
}

// parseDigestParams parses key=value pairs from a Digest auth header value.
func parseDigestParams(header string) map[string]string {
	params := make(map[string]string)

	// Skip "Digest " prefix
	if len(header) > 7 && header[:7] == "Digest " {
		header = header[7:]
	}

	// Simple parser for key="value" or key=value pairs
	i := 0
	for i < len(header) {
		// Skip whitespace and commas
		for i < len(header) && (header[i] == ' ' || header[i] == ',') {
			i++
		}
		if i >= len(header) {
			break
		}

		// Read key
		keyStart := i
		for i < len(header) && header[i] != '=' {
			i++
		}
		if i >= len(header) {
			break
		}
		key := header[keyStart:i]
		i++ // skip '='

		// Read value
		var value string
		if i < len(header) && header[i] == '"' {
			i++ // skip opening quote
			valStart := i
			for i < len(header) && header[i] != '"' {
				i++
			}
			value = header[valStart:i]
			if i < len(header) {
				i++ // skip closing quote
			}
		} else {
			valStart := i
			for i < len(header) && header[i] != ',' && header[i] != ' ' {
				i++
			}
			value = header[valStart:i]
		}

		params[key] = value
	}

	return params
}
