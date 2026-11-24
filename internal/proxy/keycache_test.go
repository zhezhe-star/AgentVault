package proxy

import (
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/zhezhe-star/AgentVault/internal/secret"
)

func TestKeyCacheTTL(t *testing.T) {
	store := secret.NewMemoryStore()
	_ = store.Set("agentvault", "prov/k1", "secret-v1")
	c := newKeyCache(store, "agentvault")

	v, err := c.get("prov", "k1")
	if err != nil || v != "secret-v1" {
		t.Fatalf("get: %v %q", err, v)
	}

	// Rotate the underlying secret: the cache must keep serving the old
	// value until the TTL passes.
	_ = store.Set("agentvault", "prov/k1", "secret-v2")
	if v, _ := c.get("prov", "k1"); v != "secret-v1" {
		t.Fatalf("cache did not honor TTL: %q", v)
	}

	// Invalidate immediately (used after upstream failures) so rotation
	// takes effect without waiting.
	c.invalidate("prov", "k1")
	if v, _ := c.get("prov", "k1"); v != "secret-v2" {
		t.Fatalf("invalidate did not refresh: %q", v)
	}
}

func TestKeyCacheMissingSecret(t *testing.T) {
	c := newKeyCache(secret.NewMemoryStore(), "agentvault")
	if _, err := c.get("prov", "nope"); !isNotFound(err) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	// A non-miss error must not be classified as not-found.
	if isNotFound(errors.New("boom")) {
		t.Fatal("plain error classified as not-found")
	}
}

// httptest.ResponseRecorder implements http.Flusher, so statusWriter must
// forward Flush to it.
func TestStatusWriterFlushForwarding(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := &statusWriter{ResponseWriter: rec, status: 200}
	sw.Flush() // must reach the recorder without panicking
	if sw.status != 200 {
		t.Fatalf("status = %d", sw.status)
	}
}
