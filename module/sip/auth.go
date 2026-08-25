package sip

import (
	"crypto/md5"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emiago/sipgo/sip"
)

const (
	defaultDigestNonceTTL = 5 * time.Minute
	maxDigestNonces       = 4096
)

// DigestVerification describes why a SIP Digest credential was accepted or
// rejected. Stale credentials receive a fresh challenge and may be retried.
type DigestVerification uint8

const (
	DigestInvalid DigestVerification = iota
	DigestValid
	DigestStale
)

type digestNonceState struct {
	expiresAt  time.Time
	used       bool
	nonceCount uint32
}

// DigestAuth provides SIP digest authentication helpers.
type DigestAuth struct {
	realm    string
	password string

	mu       sync.Mutex
	nonces   map[string]digestNonceState
	nonceTTL time.Duration
	now      func() time.Time
}

// NewDigestAuth creates a new digest auth helper.
func NewDigestAuth(realm, password string) *DigestAuth {
	return &DigestAuth{
		realm:    realm,
		password: password,
		nonces:   make(map[string]digestNonceState),
		nonceTTL: defaultDigestNonceTTL,
		now:      time.Now,
	}
}

// Challenge creates a 401 Unauthorized response with a WWW-Authenticate header.
func (d *DigestAuth) Challenge(req *sip.Request, stale ...bool) *sip.Response {
	nonce := generateNonce()
	now := d.now()
	d.mu.Lock()
	d.removeExpiredLocked(now)
	if len(d.nonces) >= maxDigestNonces {
		d.removeOldestLocked()
	}
	d.nonces[nonce] = digestNonceState{expiresAt: now.Add(d.nonceTTL)}
	d.mu.Unlock()

	resp := sip.NewResponseFromRequest(req, 401, "Unauthorized", nil)
	wwwAuth := fmt.Sprintf(`Digest realm="%s", nonce="%s", algorithm=MD5`, d.realm, nonce)
	if len(stale) > 0 && stale[0] {
		wwwAuth += ", stale=true"
	}
	resp.AppendHeader(sip.NewHeader("WWW-Authenticate", wwwAuth))
	return resp
}

// Verify checks the Authorization header against the expected credentials.
func (d *DigestAuth) Verify(req *sip.Request, expectedUsername string) DigestVerification {
	authHeader := req.GetHeader("Authorization")
	if authHeader == nil {
		return DigestInvalid
	}

	params := parseDigestParams(authHeader.Value())
	username := params["username"]
	realm := params["realm"]
	nonce := params["nonce"]
	uri := params["uri"]
	responseHash := params["response"]

	if username == "" || realm == "" || nonce == "" || uri == "" || responseHash == "" {
		return DigestInvalid
	}
	if username != expectedUsername || realm != d.realm || uri != req.Recipient.String() {
		return DigestInvalid
	}
	if algorithm := params["algorithm"]; algorithm != "" && !strings.EqualFold(algorithm, "MD5") {
		return DigestInvalid
	}

	now := d.now()
	d.mu.Lock()
	state, issued := d.nonces[nonce]
	if !issued || !now.Before(state.expiresAt) {
		delete(d.nonces, nonce)
		d.mu.Unlock()
		return DigestStale
	}
	d.mu.Unlock()

	ha1 := md5Hex(username + ":" + d.realm + ":" + d.password)
	ha2 := md5Hex(string(req.Method) + ":" + uri)
	qop := params["qop"]
	var (
		expected   string
		nonceCount uint64
	)
	if qop == "" {
		expected = md5Hex(ha1 + ":" + nonce + ":" + ha2)
	} else {
		if !strings.EqualFold(qop, "auth") || params["nc"] == "" || params["cnonce"] == "" {
			return DigestInvalid
		}
		var err error
		nonceCount, err = strconv.ParseUint(params["nc"], 16, 32)
		if err != nil || nonceCount == 0 {
			return DigestInvalid
		}
		expected = md5Hex(ha1 + ":" + nonce + ":" + params["nc"] + ":" + params["cnonce"] + ":auth:" + ha2)
	}
	if subtle.ConstantTimeCompare([]byte(strings.ToLower(responseHash)), []byte(expected)) != 1 {
		return DigestInvalid
	}

	// Consume or advance the nonce only after every request binding and digest
	// check has succeeded. The locked recheck makes concurrent replay atomic.
	d.mu.Lock()
	defer d.mu.Unlock()
	state, issued = d.nonces[nonce]
	if !issued || !now.Before(state.expiresAt) {
		delete(d.nonces, nonce)
		return DigestStale
	}
	if qop == "" {
		if state.used || state.nonceCount != 0 {
			return DigestStale
		}
		state.used = true
	} else {
		if state.used || uint32(nonceCount) <= state.nonceCount {
			return DigestStale
		}
		state.nonceCount = uint32(nonceCount)
	}
	d.nonces[nonce] = state
	return DigestValid
}

func (d *DigestAuth) removeExpiredLocked(now time.Time) {
	for nonce, state := range d.nonces {
		if !now.Before(state.expiresAt) {
			delete(d.nonces, nonce)
		}
	}
}

func (d *DigestAuth) removeOldestLocked() {
	var (
		oldestNonce string
		oldest      time.Time
	)
	for nonce, state := range d.nonces {
		if oldestNonce == "" || state.expiresAt.Before(oldest) {
			oldestNonce = nonce
			oldest = state.expiresAt
		}
	}
	delete(d.nonces, oldestNonce)
}

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func generateNonce() string {
	return rand.Text()
}

// parseDigestParams parses key=value pairs from a Digest auth header value.
func parseDigestParams(header string) map[string]string {
	params := make(map[string]string)

	// Skip "Digest " prefix.
	if len(header) > 7 && strings.EqualFold(header[:7], "Digest ") {
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
		key := strings.ToLower(strings.TrimSpace(header[keyStart:i]))
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
