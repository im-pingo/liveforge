// module/cluster/transport_test.go
package cluster

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/im-pingo/liveforge/core"
)

// mockTransport verifies interface compliance at compile time.
type mockTransport struct{}

var _ RelayTransport = (*mockTransport)(nil)

func (m *mockTransport) Scheme() string                                              { return "mock" }
func (m *mockTransport) Push(ctx context.Context, url string, s *core.Stream) error  { return nil }
func (m *mockTransport) Pull(ctx context.Context, url string, s *core.Stream) error  { return nil }
func (m *mockTransport) Close() error                                                { return nil }

func TestErrCodecMismatchIsDetectable(t *testing.T) {
	wrapped := fmt.Errorf("push failed: %w", ErrCodecMismatch)
	if !errors.Is(wrapped, ErrCodecMismatch) {
		t.Error("wrapped ErrCodecMismatch should be detectable via errors.Is")
	}
}
