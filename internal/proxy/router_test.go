package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zhezhe-star/AgentVault/internal/provider"
	"github.com/zhezhe-star/AgentVault/internal/secret"
)

// fixtureKeys registers a provider with the given key ids.
func fixtureKeys(t *testing.T, store *provider.Store, sec secret.Store, keyIDs ...string) {
	t.Helper()
	_ = store.CreateProvider(provider.Provider{ID: "mockprov", Name: "Mock", BaseURL: "http://placeholder"})
	for _, id := range keyIDs {
		_ = store.CreateKey(provider.Key{ID: id, ProviderID: "mockprov", SecretID: "ref", Status: provider.KeyActive, Weight: 1})
		_ = sec.Set("agentvault", "mockprov/"+id, "key-for-"+id)
	}
	_ = store.CreateModel(provider.Model{ID: "m1", ProviderID: "mockprov", KeyID: keyIDs[0], Name: "test-model", Upstream: "test-model"})
}

func TestFailoverOn429(t *testing.T) {
	// Upstream: first call hits key A (429), later calls hit key B (200).
	var callsWithKey atomic.Value // last Authorization header seen
	var aCalls, bCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		callsWithKey.Store(auth)
		switch auth {
		case "Bearer key-for-a":
			aCalls.Add(1)
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"message":"rate limited"}}`)
		default:
			bCalls.Add(1)
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"id":"1","object":"chat.completion","choices":[]}`)
		}
	}))
	defer upstream.Close()

	store, err := provider.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()
	sec := secret.NewMemoryStore()
	fixtureKeys(t, store, sec, "a", "b")
	// Point the provider at the real mock upstream.
	if err := store.CreateProvider(provider.Provider{ID: "mockprov2", Name: "M2", BaseURL: upstream.URL}); err != nil {
		t.Fatalf("provider2: %v", err)
	}
	_ = store.CreateKey(provider.Key{ID: "a2", ProviderID: "mockprov2", SecretID: "ref", Status: provider.KeyActive, Weight: 5})
	_ = store.CreateKey(provider.Key{ID: "b2", ProviderID: "mockprov2", SecretID: "ref", Status: provider.KeyActive, Weight: 1})
	_ = sec.Set("agentvault", "mockprov2/a2", "key-for-a")
	_ = sec.Set("agentvault", "mockprov2/b2", "key-for-b")
	_ = store.CreateModel(provider.Model{ID: "m2", ProviderID: "mockprov2", KeyID: "a2", Name: "model-2", Upstream: "x"})

	srv := New(store, sec, "agentvault")
	base := startProxy(t, srv)

	resp, err := http.Post(base+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"model-2","messages":[]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after failover", resp.StatusCode)
	}
	if aCalls.Load() != 1 || bCalls.Load() != 1 {
		t.Fatalf("calls a=%d b=%d, want 1/1 (failover)", aCalls.Load(), bCalls.Load())
	}

	// Key A must now be cooling: subsequent requests go straight to B.
	var before int32 = bCalls.Load()
	for i := 0; i < 3; i++ {
		r2, err := http.Post(base+"/v1/chat/completions", "application/json",
			strings.NewReader(`{"model":"model-2","messages":[]}`))
		if err != nil {
			t.Fatalf("post %d: %v", i, err)
		}
		r2.Body.Close()
		if r2.StatusCode != http.StatusOK {
			t.Fatalf("post %d status = %d", i, r2.StatusCode)
		}
	}
	if aCalls.Load() != 1 {
		t.Fatalf("cooled key A was retried: a=%d", aCalls.Load())
	}
	if bCalls.Load() != before+3 {
		t.Fatalf("b calls = %d, want %d", bCalls.Load(), before+3)
	}
}

func TestAllKeysExhaustedReturns429(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"message":"rate limited"}}`)
	}))
	defer upstream.Close()

	store, err := provider.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()
	sec := secret.NewMemoryStore()
	_ = store.CreateProvider(provider.Provider{ID: "p", Name: "P", BaseURL: upstream.URL})
	_ = store.CreateKey(provider.Key{ID: "only", ProviderID: "p", SecretID: "ref", Status: provider.KeyActive, Weight: 1})
	_ = sec.Set("agentvault", "p/only", "k")
	_ = store.CreateModel(provider.Model{ID: "m", ProviderID: "p", KeyID: "only", Name: "solo-model", Upstream: "x"})

	srv := New(store, sec, "agentvault")
	base := startProxy(t, srv)

	resp, err := http.Post(base+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"solo-model","messages":[]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 relayed", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("error body not JSON: %v", err)
	}
}

func TestCooldownExpiryRecoversKey(t *testing.T) {
	store, err := provider.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()
	r := newRouter(store, 10*time.Millisecond) // short cooldown for the test
	// Register one active key serving the model so pick has a candidate.
	_ = store.CreateProvider(provider.Provider{ID: "p", Name: "P", BaseURL: "http://x"})
	_ = store.CreateKey(provider.Key{ID: "k", ProviderID: "p", SecretID: "ref", Status: provider.KeyActive, Weight: 1})
	_ = store.CreateModel(provider.Model{ID: "m", ProviderID: "p", KeyID: "k", Name: "model", Upstream: "x"})
	c := candidate{providerID: "p", keyID: "k", baseURL: "http://x"}

	if _, ok := r.pick("model"); !ok {
		t.Fatal("pick failed with empty state")
	}
	r.fail(c)
	if _, ok := r.pick("model"); ok {
		t.Fatal("cooling key was picked")
	}
	if r.coolingCount() != 1 {
		t.Fatalf("coolingCount = %d, want 1", r.coolingCount())
	}
	time.Sleep(20 * time.Millisecond)
	if _, ok := r.pick("model"); !ok {
		t.Fatal("key did not recover after cooldown expiry")
	}
}
