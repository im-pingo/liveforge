// module/cluster/registry_test.go
package cluster

import (
	"testing"
)

func TestRegistryRegisterAndResolve(t *testing.T) {
	r := NewTransportRegistry()
	r.Register(&mockTransport{})

	tr, err := r.Resolve("mock://host/path")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if tr.Scheme() != "mock" {
		t.Errorf("Scheme = %q, want %q", tr.Scheme(), "mock")
	}
}

func TestRegistryResolveUnknownScheme(t *testing.T) {
	r := NewTransportRegistry()

	_, err := r.Resolve("unknown://host/path")
	if err == nil {
		t.Fatal("expected error for unknown scheme")
	}
}

func TestRegistryResolveNoScheme(t *testing.T) {
	r := NewTransportRegistry()

	_, err := r.Resolve("host/path")
	if err == nil {
		t.Fatal("expected error for URL without scheme")
	}
}

func TestRegistryResolveMultipleTransports(t *testing.T) {
	r := NewTransportRegistry()
	r.Register(&mockTransport{})

	mock2 := &mockTransportAlt{}
	r.Register(mock2)

	tr, err := r.Resolve("alt://host/path")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if tr.Scheme() != "alt" {
		t.Errorf("Scheme = %q, want %q", tr.Scheme(), "alt")
	}
}

type mockTransportAlt struct{ mockTransport }

func (m *mockTransportAlt) Scheme() string { return "alt" }
