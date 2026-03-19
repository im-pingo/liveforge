package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
)

// makeJWT creates a HS256 JWT for testing.
func makeJWT(secret string, claims jwtClaims) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload, _ := json.Marshal(claims)
	payloadEnc := base64.RawURLEncoding.EncodeToString(payload)

	signingInput := header + "." + payloadEnc
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signingInput))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	return signingInput + "." + sig
}

func TestCheckToken_Valid(t *testing.T) {
	secret := "test-secret-key"
	token := makeJWT(secret, jwtClaims{
		Sub:    "live/test",
		Action: "publish",
		Exp:    time.Now().Add(time.Hour).Unix(),
	})

	err := checkToken(secret, token, "publish", "live/test")
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestCheckToken_Expired(t *testing.T) {
	secret := "test-secret-key"
	token := makeJWT(secret, jwtClaims{
		Sub:    "live/test",
		Action: "publish",
		Exp:    time.Now().Add(-time.Hour).Unix(),
	})

	err := checkToken(secret, token, "publish", "live/test")
	if err != ErrTokenExpired {
		t.Fatalf("expected ErrTokenExpired, got %v", err)
	}
}

func TestCheckToken_BadSignature(t *testing.T) {
	token := makeJWT("correct-secret", jwtClaims{
		Exp: time.Now().Add(time.Hour).Unix(),
	})

	err := checkToken("wrong-secret", token, "publish", "live/test")
	if err != ErrTokenInvalid {
		t.Fatalf("expected ErrTokenInvalid, got %v", err)
	}
}

func TestCheckToken_StreamMismatch(t *testing.T) {
	secret := "test-secret-key"
	token := makeJWT(secret, jwtClaims{
		Sub:    "live/other",
		Action: "publish",
		Exp:    time.Now().Add(time.Hour).Unix(),
	})

	err := checkToken(secret, token, "publish", "live/test")
	if err != ErrStreamMismatch {
		t.Fatalf("expected ErrStreamMismatch, got %v", err)
	}
}

func TestCheckToken_ActionMismatch(t *testing.T) {
	secret := "test-secret-key"
	token := makeJWT(secret, jwtClaims{
		Action: "subscribe",
		Exp:    time.Now().Add(time.Hour).Unix(),
	})

	err := checkToken(secret, token, "publish", "live/test")
	if err != ErrActionMismatch {
		t.Fatalf("expected ErrActionMismatch, got %v", err)
	}
}

func TestCheckToken_Empty(t *testing.T) {
	err := checkToken("secret", "", "publish", "live/test")
	if err != ErrUnauthorized {
		t.Fatalf("expected ErrUnauthorized, got %v", err)
	}
}

func TestCheckToken_NoSubNoAction(t *testing.T) {
	secret := "test-secret-key"
	token := makeJWT(secret, jwtClaims{
		Exp: time.Now().Add(time.Hour).Unix(),
	})

	// Should pass — sub and action are optional
	err := checkToken(secret, token, "publish", "live/test")
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestCheckCallback_Accept(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx := &core.EventContext{
		StreamKey:  "live/test",
		Protocol:   "rtmp",
		RemoteAddr: "127.0.0.1:1234",
	}
	err := checkCallback(srv.URL, 3*time.Second, ctx, "publish")
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestCheckCallback_Reject(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	ctx := &core.EventContext{
		StreamKey:  "live/test",
		Protocol:   "rtmp",
		RemoteAddr: "127.0.0.1:1234",
	}
	err := checkCallback(srv.URL, 3*time.Second, ctx, "publish")
	if err != ErrCallbackRejected {
		t.Fatalf("expected ErrCallbackRejected, got %v", err)
	}
}

func TestCheckCallback_EmptyURL(t *testing.T) {
	ctx := &core.EventContext{StreamKey: "live/test"}
	err := checkCallback("", 3*time.Second, ctx, "publish")
	if err != ErrUnauthorized {
		t.Fatalf("expected ErrUnauthorized, got %v", err)
	}
}

func TestCheckAuth_ModeNone(t *testing.T) {
	rule := config.AuthRuleConfig{Mode: "none"}
	ctx := &core.EventContext{StreamKey: "live/test"}
	if err := checkAuth(rule, ctx, "publish"); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestCheckAuth_ModeEmpty(t *testing.T) {
	rule := config.AuthRuleConfig{Mode: ""}
	ctx := &core.EventContext{StreamKey: "live/test"}
	if err := checkAuth(rule, ctx, "publish"); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestCheckAuth_ModeToken(t *testing.T) {
	secret := "my-secret"
	token := makeJWT(secret, jwtClaims{
		Action: "publish",
		Exp:    time.Now().Add(time.Hour).Unix(),
	})

	rule := config.AuthRuleConfig{
		Mode:  "token",
		Token: config.TokenConfig{Secret: secret, Algorithm: "HS256"},
	}
	ctx := &core.EventContext{
		StreamKey: "live/test",
		Params:    map[string]string{"token": token},
	}
	if err := checkAuth(rule, ctx, "publish"); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestCheckAuth_ModeTokenReject(t *testing.T) {
	rule := config.AuthRuleConfig{
		Mode:  "token",
		Token: config.TokenConfig{Secret: "secret"},
	}
	ctx := &core.EventContext{
		StreamKey: "live/test",
		Params:    map[string]string{"token": "invalid.token.here"},
	}
	if err := checkAuth(rule, ctx, "publish"); err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestCheckAuth_ModeCombined_TokenFails_CallbackSucceeds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	rule := config.AuthRuleConfig{
		Mode:  "token+callback",
		Token: config.TokenConfig{Secret: "secret"},
		Callback: config.CallbackConfig{
			URL:     srv.URL,
			Timeout: 3 * time.Second,
		},
	}
	ctx := &core.EventContext{
		StreamKey: "live/test",
		Params:    map[string]string{"token": "bad-token"},
	}
	if err := checkAuth(rule, ctx, "publish"); err != nil {
		t.Fatalf("expected nil (callback fallback), got %v", err)
	}
}

func TestCheckAuth_ModeCombined_TokenSucceeds(t *testing.T) {
	secret := "my-secret"
	token := makeJWT(secret, jwtClaims{
		Exp: time.Now().Add(time.Hour).Unix(),
	})

	// Callback server that would reject — should never be called
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	rule := config.AuthRuleConfig{
		Mode:  "token+callback",
		Token: config.TokenConfig{Secret: secret},
		Callback: config.CallbackConfig{
			URL:     srv.URL,
			Timeout: 3 * time.Second,
		},
	}
	ctx := &core.EventContext{
		StreamKey: "live/test",
		Params:    map[string]string{"token": token},
	}
	if err := checkAuth(rule, ctx, "publish"); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

// TestEventBusAuthIntegration verifies that auth hooks reject EventPublish via the EventBus.
func TestEventBusAuthIntegration(t *testing.T) {
	bus := core.NewEventBus()

	m := &Module{
		cfg: config.AuthConfig{
			Enabled: true,
			Publish: config.AuthRuleConfig{
				Mode:  "token",
				Token: config.TokenConfig{Secret: "test-secret"},
			},
		},
	}

	for _, h := range m.Hooks() {
		bus.Register(h)
	}

	// No token → should be rejected
	err := bus.Emit(core.EventPublish, &core.EventContext{
		StreamKey:  "live/test",
		Protocol:   "rtmp",
		RemoteAddr: "127.0.0.1:1234",
	})
	if err == nil {
		t.Fatal("expected auth rejection, got nil")
	}

	// Valid token → should pass
	token := makeJWT("test-secret", jwtClaims{
		Action: "publish",
		Exp:    time.Now().Add(time.Hour).Unix(),
	})
	err = bus.Emit(core.EventPublish, &core.EventContext{
		StreamKey:  "live/test",
		Protocol:   "rtmp",
		RemoteAddr: "127.0.0.1:1234",
		Params:     map[string]string{"token": token},
	})
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestCheckAuth_UnknownMode(t *testing.T) {
	rule := config.AuthRuleConfig{Mode: "magic"}
	ctx := &core.EventContext{StreamKey: "live/test"}
	err := checkAuth(rule, ctx, "publish")
	if err == nil {
		t.Fatal("expected error for unknown mode")
	}
	expected := fmt.Sprintf("unknown auth mode: %s", "magic")
	if err.Error() != expected {
		t.Fatalf("expected %q, got %q", expected, err.Error())
	}
}
