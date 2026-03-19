package auth

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
)

var (
	ErrUnauthorized    = errors.New("unauthorized")
	ErrTokenExpired    = errors.New("token expired")
	ErrTokenInvalid    = errors.New("invalid token")
	ErrStreamMismatch  = errors.New("token stream key mismatch")
	ErrActionMismatch  = errors.New("token action mismatch")
	ErrCallbackRejected = errors.New("auth callback rejected")
)

// jwtClaims holds the expected JWT payload fields.
type jwtClaims struct {
	Sub    string `json:"sub"`    // stream key (optional)
	Action string `json:"action"` // "publish" or "subscribe"
	Exp    int64  `json:"exp"`    // expiry unix timestamp
}

// callbackRequest is the JSON body sent to the auth callback URL.
type callbackRequest struct {
	StreamKey  string `json:"stream_key"`
	Protocol   string `json:"protocol"`
	RemoteAddr string `json:"remote_addr"`
	Token      string `json:"token,omitempty"`
	Action     string `json:"action"`
}

// checkAuth dispatches to the appropriate auth method based on mode.
func checkAuth(rule config.AuthRuleConfig, ctx *core.EventContext, action string) error {
	switch rule.Mode {
	case "", "none":
		return nil
	case "token":
		return checkToken(rule.Token.Secret, getParam(ctx, "token"), action, ctx.StreamKey)
	case "callback":
		return checkCallback(rule.Callback.URL, rule.Callback.Timeout, ctx, action)
	case "token+callback":
		err := checkToken(rule.Token.Secret, getParam(ctx, "token"), action, ctx.StreamKey)
		if err == nil {
			return nil
		}
		return checkCallback(rule.Callback.URL, rule.Callback.Timeout, ctx, action)
	default:
		return fmt.Errorf("unknown auth mode: %s", rule.Mode)
	}
}

// getParam safely extracts a param from EventContext.
func getParam(ctx *core.EventContext, key string) string {
	if ctx.Params == nil {
		return ""
	}
	return ctx.Params[key]
}

// checkToken verifies a JWT token using HMAC-SHA256.
func checkToken(secret, token, action, streamKey string) error {
	if token == "" {
		return ErrUnauthorized
	}

	claims, err := parseAndVerifyJWT(secret, token)
	if err != nil {
		return err
	}

	// Check expiry
	if claims.Exp > 0 && time.Now().Unix() > claims.Exp {
		return ErrTokenExpired
	}

	// Check action if present
	if claims.Action != "" && claims.Action != action {
		return ErrActionMismatch
	}

	// Check stream key if present
	if claims.Sub != "" && claims.Sub != streamKey {
		return ErrStreamMismatch
	}

	return nil
}

// parseAndVerifyJWT decodes and verifies a HS256 JWT without external libraries.
func parseAndVerifyJWT(secret, token string) (*jwtClaims, error) {
	parts := strings.SplitN(token, ".", 3)
	if len(parts) != 3 {
		return nil, ErrTokenInvalid
	}

	// Verify signature: HMAC-SHA256(header.payload, secret)
	signingInput := parts[0] + "." + parts[1]
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signingInput))
	expectedSig := mac.Sum(nil)

	actualSig, err := base64URLDecode(parts[2])
	if err != nil {
		return nil, ErrTokenInvalid
	}

	if !hmac.Equal(expectedSig, actualSig) {
		return nil, ErrTokenInvalid
	}

	// Decode payload
	payload, err := base64URLDecode(parts[1])
	if err != nil {
		return nil, ErrTokenInvalid
	}

	var claims jwtClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, ErrTokenInvalid
	}

	return &claims, nil
}

// base64URLDecode decodes base64url without padding.
func base64URLDecode(s string) ([]byte, error) {
	// Add padding if needed
	switch len(s) % 4 {
	case 2:
		s += "=="
	case 3:
		s += "="
	}
	return base64.URLEncoding.DecodeString(s)
}

// checkCallback sends a POST request to the auth callback URL.
func checkCallback(url string, timeout time.Duration, ctx *core.EventContext, action string) error {
	if url == "" {
		return ErrUnauthorized
	}

	if timeout == 0 {
		timeout = 3 * time.Second
	}

	body := callbackRequest{
		StreamKey:  ctx.StreamKey,
		Protocol:   ctx.Protocol,
		RemoteAddr: ctx.RemoteAddr,
		Token:      getParam(ctx, "token"),
		Action:     action,
	}

	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("auth callback marshal: %w", err)
	}

	client := &http.Client{Timeout: timeout}
	resp, err := client.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("auth callback request: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusOK {
		return ErrCallbackRejected
	}

	return nil
}
