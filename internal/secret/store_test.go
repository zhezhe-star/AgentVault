package secret

import (
	"errors"
	"testing"
)

// TestMemoryStoreRoundTrip exercises the Store contract against the memory
// backend (the same contract the keyring backend must satisfy; the keyring
// itself cannot be touched from CI).
func TestMemoryStoreRoundTrip(t *testing.T) {
	s := NewMemoryStore()

	if err := s.Set(Service, "bigmodel/key-1", "test-secret-value"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := s.Get(Service, "bigmodel/key-1")
	if err != nil || got != "test-secret-value" {
		t.Fatalf("Get: %v %q", err, got)
	}

	// Accounts are isolated from each other.
	if _, err := s.Get(Service, "bigmodel/key-2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(other account) err = %v, want ErrNotFound", err)
	}
	// Services are isolated too.
	if _, err := s.Get("other-service", "bigmodel/key-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(other service) err = %v, want ErrNotFound", err)
	}

	if err := s.Delete(Service, "bigmodel/key-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(Service, "bigmodel/key-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after delete err = %v, want ErrNotFound", err)
	}
	if err := s.Delete(Service, "bigmodel/key-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete twice err = %v, want ErrNotFound", err)
	}
}
