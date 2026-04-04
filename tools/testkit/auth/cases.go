package auth

import "time"

// TestCase describes a single authentication test scenario.
type TestCase struct {
	Name        string // human-readable label, e.g. "valid", "expired"
	Token       string // the JWT string, or "" for "missing" case
	ExpectAllow bool   // true if the server should accept this token
}

// GenerateCases returns the standard set of auth test cases for a given action.
// The six cases are: valid, expired, wrong_secret, missing, wrong_stream, wrong_action.
func GenerateCases(secret, streamKey, action string) []TestCase {
	// Determine the opposite action for the wrong_action case.
	oppositeAction := "subscribe"
	if action == "subscribe" {
		oppositeAction = "publish"
	}

	return []TestCase{
		{
			Name:        "valid",
			Token:       GenerateJWT(secret, streamKey, action, time.Now().Add(time.Hour)),
			ExpectAllow: true,
		},
		{
			Name:        "expired",
			Token:       GenerateJWT(secret, streamKey, action, time.Now().Add(-time.Hour)),
			ExpectAllow: false,
		},
		{
			Name:        "wrong_secret",
			Token:       GenerateJWT("not-the-real-secret", streamKey, action, time.Now().Add(time.Hour)),
			ExpectAllow: false,
		},
		{
			Name:        "missing",
			Token:       "",
			ExpectAllow: false,
		},
		{
			Name:        "wrong_stream",
			Token:       GenerateJWT(secret, "live/wrong-stream-key", action, time.Now().Add(time.Hour)),
			ExpectAllow: false,
		},
		{
			Name:        "wrong_action",
			Token:       GenerateJWT(secret, streamKey, oppositeAction, time.Now().Add(time.Hour)),
			ExpectAllow: false,
		},
	}
}
