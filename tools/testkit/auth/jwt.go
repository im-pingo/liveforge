// Package auth provides an authentication test suite for LiveForge.
// It generates JWT test cases (valid, expired, wrong secret, missing, etc.),
// runs them against each protocol's auth mechanism, and produces an AuthReport.
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"time"
)

// jwtClaims mirrors the server-side JWT claims structure.
type jwtClaims struct {
	Sub    string `json:"sub,omitempty"`    // stream key (optional)
	Action string `json:"action,omitempty"` // "publish" or "subscribe" (optional)
	Exp    int64  `json:"exp,omitempty"`    // expiry unix timestamp (optional)
}

// GenerateJWT creates an HS256 JWT token with the given claims.
// The format matches the server's parseAndVerifyJWT expectations:
// base64url(header).base64url(payload).base64url(HMAC-SHA256(header.payload, secret))
func GenerateJWT(secret, sub, action string, exp time.Time) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))

	claims := jwtClaims{
		Sub:    sub,
		Action: action,
	}
	if !exp.IsZero() {
		claims.Exp = exp.Unix()
	}

	payload, _ := json.Marshal(claims)
	payloadEnc := base64.RawURLEncoding.EncodeToString(payload)

	signingInput := header + "." + payloadEnc
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signingInput))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	return signingInput + "." + sig
}
