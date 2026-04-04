package auth

import (
	"context"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/tools/testkit/testutil"
)

func TestGenerateJWT_RoundTrip(t *testing.T) {
	secret := "test-secret"
	token := GenerateJWT(secret, "live/test", "publish", time.Now().Add(time.Hour))
	if token == "" {
		t.Fatal("GenerateJWT returned empty string")
	}

	// Token must have three dot-separated parts.
	parts := 0
	for i := range token {
		if token[i] == '.' {
			parts++
		}
	}
	if parts != 2 {
		t.Fatalf("expected 2 dots in JWT, got %d", parts)
	}
}

func TestGenerateCases_Count(t *testing.T) {
	cases := GenerateCases("secret", "live/test", "publish")
	if len(cases) != 6 {
		t.Fatalf("expected 6 test cases, got %d", len(cases))
	}

	// Exactly one case should be allowed (the "valid" case).
	allowed := 0
	for _, c := range cases {
		if c.ExpectAllow {
			allowed++
		}
	}
	if allowed != 1 {
		t.Fatalf("expected 1 allowed case, got %d", allowed)
	}
}

func TestGenerateCases_Names(t *testing.T) {
	cases := GenerateCases("secret", "live/test", "publish")
	expected := map[string]bool{
		"valid":        false,
		"expired":      false,
		"wrong_secret": false,
		"missing":      false,
		"wrong_stream": false,
		"wrong_action": false,
	}
	for _, c := range cases {
		if _, ok := expected[c.Name]; !ok {
			t.Errorf("unexpected case name: %q", c.Name)
		}
		expected[c.Name] = true
	}
	for name, seen := range expected {
		if !seen {
			t.Errorf("missing case: %q", name)
		}
	}
}

func TestGenerateCases_MissingHasEmptyToken(t *testing.T) {
	cases := GenerateCases("secret", "live/test", "publish")
	for _, c := range cases {
		if c.Name == "missing" && c.Token != "" {
			t.Error("missing case should have empty token")
		}
	}
}

func TestAuthMatrix(t *testing.T) {
	secret := "test-secret-for-auth"
	srv := testutil.StartTestServer(t,
		testutil.WithRTMP(),
		testutil.WithHTTPStream(),
		testutil.WithAuth(secret),
	)

	// Test RTMP publish auth and HTTP-FLV subscribe auth.
	// SRT is excluded because the SRT module does not currently pass token
	// query params from the streamid to EventContext.Params, so token-based
	// auth always fails. SRT auth testing can be added once the SRT module
	// is updated to forward tokens.
	cfg := AuthTestConfig{
		ServerAddrs: map[string]string{
			"rtmp": srv.RTMPAddr(),
			"http": srv.HTTPAddr(),
		},
		Secret:    secret,
		StreamKey: "live/auth-test",
		Protocols: []string{"rtmp", "http"},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	rpt, err := RunAuthTests(ctx, cfg)
	if err != nil {
		t.Fatalf("RunAuthTests: %v", err)
	}

	t.Logf("Auth test results: total=%d passed=%d failed=%d", rpt.Total, rpt.Passed, rpt.Failed)

	for _, c := range rpt.Cases {
		t.Logf("  %s %s %s: expect_allow=%v actual_allow=%v pass=%v latency=%dms err=%s",
			c.Protocol, c.Action, c.Credential, c.ExpectAllow, c.ActualAllow, c.Pass, c.LatencyMs, c.Error)
	}

	if rpt.Failed > 0 {
		for _, c := range rpt.Cases {
			if !c.Pass {
				t.Errorf("FAIL: %s %s %s: expect_allow=%v actual_allow=%v err=%s",
					c.Protocol, c.Action, c.Credential, c.ExpectAllow, c.ActualAllow, c.Error)
			}
		}
	}
}
