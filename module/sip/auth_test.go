package sip

import (
	"fmt"
	"testing"

	"github.com/emiago/sipgo/sip"
)

func TestDigestAuthChallenge(t *testing.T) {
	auth := NewDigestAuth("3402000000", "12345678")

	req := sip.NewRequest(sip.REGISTER, sip.Uri{
		Host: "192.168.1.100",
		Port: 5060,
	})
	req.AppendHeader(&sip.FromHeader{
		DisplayName: "test",
		Address:     sip.Uri{User: "34020000001320000001", Host: "3402000000"},
	})

	resp := auth.Challenge(req)
	if resp.StatusCode != 401 {
		t.Errorf("StatusCode = %d, want 401", resp.StatusCode)
	}

	wwwAuth := resp.GetHeader("WWW-Authenticate")
	if wwwAuth == nil {
		t.Fatal("missing WWW-Authenticate header")
	}
	if wwwAuth.Value()[:6] != "Digest" {
		t.Errorf("WWW-Authenticate value = %q, want Digest prefix", wwwAuth.Value())
	}
}

func TestDigestAuthVerify(t *testing.T) {
	realm := "3402000000"
	password := "12345678"
	auth := NewDigestAuth(realm, password)

	username := "34020000001320000001"
	nonce := "abc123"
	uri := "sip:34020000002000000001@3402000000"
	method := "REGISTER"

	// Compute expected response
	ha1 := md5Hex(username + ":" + realm + ":" + password)
	ha2 := md5Hex(method + ":" + uri)
	response := md5Hex(ha1 + ":" + nonce + ":" + ha2)

	req := sip.NewRequest(sip.REGISTER, sip.Uri{Host: "192.168.1.100", Port: 5060})
	authValue := fmt.Sprintf(
		`Digest username="%s", realm="%s", nonce="%s", uri="%s", response="%s", algorithm=MD5`,
		username, realm, nonce, uri, response,
	)
	req.AppendHeader(sip.NewHeader("Authorization", authValue))

	if !auth.Verify(req) {
		t.Error("expected verification to succeed")
	}

	// Test with wrong password
	auth2 := NewDigestAuth(realm, "wrongpassword")
	if auth2.Verify(req) {
		t.Error("expected verification to fail with wrong password")
	}
}

func TestDigestAuthVerifyMissingHeader(t *testing.T) {
	auth := NewDigestAuth("realm", "pass")
	req := sip.NewRequest(sip.REGISTER, sip.Uri{Host: "localhost", Port: 5060})
	if auth.Verify(req) {
		t.Error("expected false for missing Authorization header")
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

	for k, v := range expected {
		if params[k] != v {
			t.Errorf("params[%q] = %q, want %q", k, params[k], v)
		}
	}
}
