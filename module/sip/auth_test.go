package sip

import (
	"fmt"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
)

const (
	testDigestRealm    = "3402000000"
	testDigestPassword = "12345678"
	testDigestDevice   = "34020000001320000001"
)

func newDigestRequest(method sip.RequestMethod, username string) *sip.Request {
	req := sip.NewRequest(method, sip.Uri{User: "34020000002000000001", Host: testDigestRealm})
	req.AppendHeader(&sip.FromHeader{
		DisplayName: username,
		Address:     sip.Uri{User: username, Host: testDigestRealm},
	})
	return req
}

func challengeParams(t *testing.T, resp *sip.Response) map[string]string {
	t.Helper()
	if resp.StatusCode != 401 {
		t.Fatalf("challenge status = %d, want 401", resp.StatusCode)
	}
	header := resp.GetHeader("WWW-Authenticate")
	if header == nil {
		t.Fatal("challenge missing WWW-Authenticate")
	}
	return parseDigestParams(header.Value())
}

func addDigestAuthorization(req *sip.Request, username, realm, password, nonce string) {
	uri := req.Recipient.String()
	ha1 := md5Hex(username + ":" + realm + ":" + password)
	ha2 := md5Hex(string(req.Method) + ":" + uri)
	response := md5Hex(ha1 + ":" + nonce + ":" + ha2)
	req.AppendHeader(sip.NewHeader("Authorization", fmt.Sprintf(
		`Digest username="%s", realm="%s", nonce="%s", uri="%s", response="%s", algorithm=MD5`,
		username, realm, nonce, uri, response,
	)))
}

func addQOPDigestAuthorization(req *sip.Request, username, realm, password, nonce, nc, cnonce string) {
	uri := req.Recipient.String()
	ha1 := md5Hex(username + ":" + realm + ":" + password)
	ha2 := md5Hex(string(req.Method) + ":" + uri)
	response := md5Hex(ha1 + ":" + nonce + ":" + nc + ":" + cnonce + ":auth:" + ha2)
	req.AppendHeader(sip.NewHeader("Authorization", fmt.Sprintf(
		`Digest username="%s", realm="%s", nonce="%s", uri="%s", response="%s", algorithm=MD5, qop=auth, nc=%s, cnonce="%s"`,
		username, realm, nonce, uri, response, nc, cnonce,
	)))
}

func TestDigestAuthChallengeDerivedCredentialSucceedsOnce(t *testing.T) {
	auth := NewDigestAuth(testDigestRealm, testDigestPassword)
	req := newDigestRequest(sip.REGISTER, testDigestDevice)
	nonce := challengeParams(t, auth.Challenge(req))["nonce"]
	if nonce == "" {
		t.Fatal("challenge nonce is empty")
	}

	addDigestAuthorization(req, testDigestDevice, testDigestRealm, testDigestPassword, nonce)
	if got := auth.Verify(req, testDigestDevice); got != DigestValid {
		t.Fatalf("challenge-derived verification = %v, want valid", got)
	}
	if got := auth.Verify(req, testDigestDevice); got != DigestStale {
		t.Fatalf("exact replay verification = %v, want stale", got)
	}
}

func TestDigestAuthRejectsNonceItDidNotIssue(t *testing.T) {
	auth := NewDigestAuth(testDigestRealm, testDigestPassword)
	req := newDigestRequest(sip.REGISTER, testDigestDevice)
	addDigestAuthorization(req, testDigestDevice, testDigestRealm, testDigestPassword, "attacker-nonce")

	if got := auth.Verify(req, testDigestDevice); got != DigestStale {
		t.Fatalf("arbitrary nonce verification = %v, want stale", got)
	}
}

func TestDigestAuthRejectsExpiredNonceAsStale(t *testing.T) {
	now := time.Unix(100, 0)
	auth := NewDigestAuth(testDigestRealm, testDigestPassword)
	auth.now = func() time.Time { return now }
	auth.nonceTTL = time.Minute
	req := newDigestRequest(sip.REGISTER, testDigestDevice)
	nonce := challengeParams(t, auth.Challenge(req))["nonce"]
	addDigestAuthorization(req, testDigestDevice, testDigestRealm, testDigestPassword, nonce)

	now = now.Add(time.Minute + time.Nanosecond)
	if got := auth.Verify(req, testDigestDevice); got != DigestStale {
		t.Fatalf("expired nonce verification = %v, want stale", got)
	}
	if stale := challengeParams(t, auth.Challenge(req, true))["stale"]; stale != "true" {
		t.Fatalf("stale challenge marker = %q, want true", stale)
	}
}

func TestDigestAuthBindsIdentityRealmMethodAndRequestURI(t *testing.T) {
	tests := []struct {
		name       string
		method     sip.RequestMethod
		identity   string
		username   string
		realm      string
		mutateURI  bool
		hashMethod sip.RequestMethod
	}{
		{name: "identity", method: sip.REGISTER, identity: "other-device", username: testDigestDevice, realm: testDigestRealm, hashMethod: sip.REGISTER},
		{name: "username", method: sip.REGISTER, identity: testDigestDevice, username: "other-device", realm: testDigestRealm, hashMethod: sip.REGISTER},
		{name: "realm", method: sip.REGISTER, identity: testDigestDevice, username: testDigestDevice, realm: "other-realm", hashMethod: sip.REGISTER},
		{name: "method", method: sip.REGISTER, identity: testDigestDevice, username: testDigestDevice, realm: testDigestRealm, hashMethod: sip.MESSAGE},
		{name: "request URI", method: sip.REGISTER, identity: testDigestDevice, username: testDigestDevice, realm: testDigestRealm, mutateURI: true, hashMethod: sip.REGISTER},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			auth := NewDigestAuth(testDigestRealm, testDigestPassword)
			req := newDigestRequest(test.method, testDigestDevice)
			nonce := challengeParams(t, auth.Challenge(req))["nonce"]
			uri := req.Recipient.String()
			if test.mutateURI {
				uri = "sip:other@" + testDigestRealm
			}
			ha1 := md5Hex(test.username + ":" + test.realm + ":" + testDigestPassword)
			ha2 := md5Hex(string(test.hashMethod) + ":" + uri)
			response := md5Hex(ha1 + ":" + nonce + ":" + ha2)
			req.AppendHeader(sip.NewHeader("Authorization", fmt.Sprintf(
				`Digest username="%s", realm="%s", nonce="%s", uri="%s", response="%s", algorithm=MD5`,
				test.username, test.realm, nonce, uri, response,
			)))

			if got := auth.Verify(req, test.identity); got != DigestInvalid {
				t.Fatalf("mismatched %s verification = %v, want invalid", test.name, got)
			}
		})
	}
}

func TestDigestAuthQOPNonceCountMustIncrease(t *testing.T) {
	auth := NewDigestAuth(testDigestRealm, testDigestPassword)
	first := newDigestRequest(sip.REGISTER, testDigestDevice)
	nonce := challengeParams(t, auth.Challenge(first))["nonce"]
	addQOPDigestAuthorization(first, testDigestDevice, testDigestRealm, testDigestPassword, nonce, "00000001", "client")
	if got := auth.Verify(first, testDigestDevice); got != DigestValid {
		t.Fatalf("first qop verification = %v, want valid", got)
	}

	second := newDigestRequest(sip.REGISTER, testDigestDevice)
	addQOPDigestAuthorization(second, testDigestDevice, testDigestRealm, testDigestPassword, nonce, "00000002", "client")
	if got := auth.Verify(second, testDigestDevice); got != DigestValid {
		t.Fatalf("increased nonce-count verification = %v, want valid", got)
	}

	replay := newDigestRequest(sip.REGISTER, testDigestDevice)
	addQOPDigestAuthorization(replay, testDigestDevice, testDigestRealm, testDigestPassword, nonce, "00000002", "client")
	if got := auth.Verify(replay, testDigestDevice); got != DigestStale {
		t.Fatalf("replayed nonce-count verification = %v, want stale", got)
	}
}

func TestDigestAuthVerifyMissingHeader(t *testing.T) {
	auth := NewDigestAuth(testDigestRealm, testDigestPassword)
	if got := auth.Verify(newDigestRequest(sip.REGISTER, testDigestDevice), testDigestDevice); got != DigestInvalid {
		t.Fatalf("missing header verification = %v, want invalid", got)
	}
}

func TestParseDigestParams(t *testing.T) {
	header := `Digest username="alice", realm="biloxi.com", nonce="dcd98b", uri="sip:bob@biloxi.com", response="6629fae"`
	params := parseDigestParams(header)

	expected := map[string]string{
		"username": "alice",
		"realm":    "biloxi.com",
		"nonce":    "dcd98b",
		"uri":      "sip:bob@biloxi.com",
		"response": "6629fae",
	}
	for key, want := range expected {
		if got := params[key]; got != want {
			t.Errorf("params[%q] = %q, want %q", key, got, want)
		}
	}
}
