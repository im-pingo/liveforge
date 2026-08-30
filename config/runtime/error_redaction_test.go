package runtime

import (
	"errors"
	"strings"
	"testing"
)

func TestRedactErrorHidesURLPathCredentialsAndPreservesHost(t *testing.T) {
	const rawURL = "https://hooks.slack.com/services/T111/B111/error-path-token"
	redacted := RedactError(errors.New("send webhook " + rawURL + ": connection refused"))
	for _, secret := range []string{"T111", "B111", "error-path-token"} {
		if strings.Contains(redacted, secret) {
			t.Fatalf("redacted error leaked URL path credential %q: %s", secret, redacted)
		}
	}
	if !strings.Contains(redacted, "hooks.slack.com") {
		t.Fatalf("redacted error lost safe URL host: %s", redacted)
	}
}
