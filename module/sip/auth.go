package sip

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/emiago/sipgo/sip"
)

// DigestAuth provides SIP digest authentication helpers.
type DigestAuth struct {
	realm    string
	password string
}

// NewDigestAuth creates a new digest auth helper.
func NewDigestAuth(realm, password string) *DigestAuth {
	return &DigestAuth{realm: realm, password: password}
}

// Challenge creates a 401 Unauthorized response with a WWW-Authenticate header.
func (d *DigestAuth) Challenge(req *sip.Request) *sip.Response {
	nonce := generateNonce()
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

	if username == "" || nonce == "" || uri == "" || responseHash == "" {
		return false
	}

	// Compute expected response: MD5(HA1:nonce:HA2)
	// HA1 = MD5(username:realm:password)
	// HA2 = MD5(method:uri)
	ha1 := md5Hex(username + ":" + d.realm + ":" + d.password)
	ha2 := md5Hex(string(req.Method) + ":" + uri)
	expected := md5Hex(ha1 + ":" + nonce + ":" + ha2)

	return responseHash == expected
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
