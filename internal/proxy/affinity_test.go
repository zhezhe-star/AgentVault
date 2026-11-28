package proxy

// Tests for session cache affinity (design §8, M6-4): header priority and
// validation, the in-memory binding table's TTL and health invalidation,
// and the end-to-end sticky/rebind behavior through the forwarding loop.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zhezhe-star/AgentVault/internal/auto"
	"github.com/zhezhe-star/AgentVault/internal/provider"
)

// TestSessionIDPriority covers the header priority chain (design §8):
// Claude Code > OpenCode parent > OpenCode child > generic session-id,
// with the ≥8-char validation gate.
func TestSessionIDPriority(t *testing.T) {
	h := http.Header{}
	if SessionID(h) != "" {
		t.Fatal("no headers must yield no session")
	}
	h.Set("Session-Id", "generic-session-01")
	if got := SessionID(h); got != "generic-session-01" {
		t.Fatalf("generic: %q", got)
	}
	h.Set("X-Session-Id", "child-session-002")
	if got := SessionID(h); got != "child-session-002" {
		t.Fatalf("opencode child: %q", got)
	}
	h.Set("X-Parent-Session-Id", "parent-session-03")
	if got := SessionID(h); got != "parent-session-03" {
		t.Fatalf("opencode parent must win over child: %q", got)
	}
	h.Set("X-Claude-Code-Session-Id", "claude-code-session-1")
	if got := SessionID(h); got != "claude-code-session-1" {
		t.Fatalf("claude code must win over everything: %q", got)
	}
	// Validation: too-short or junk values are ignored (falls through the
	// priority chain).
	junk := http.Header{}
	junk.Set("X-Claude-Code-Session-Id", "ab") // <8 chars
	junk.Set("Session-Id", "good-session-01")
	if got := SessionID(junk); got != "good-session-01" {
		t.Fatalf("short id must be skipped, got %q", got)
	}
	junk2 := http.Header{}
	junk2.Set("Session-Id", "has space!!1")
	if SessionID(junk2) != "" {
		t.Fatal("junk value must yield no session")
	}
}

// TestAffinityTTLExpiry: bindings die after the TTL (injectable clock).
func TestAffinityTTLExpiry(t *testing.T) {
	store, _, _ := fixture(t, "http://127.0.0.1:1")
	rt := newRouter(store, DefaultCooldown)
	a := newSessionAffinity(rt, time.Hour)
	now := time.Unix(1_000_000, 0)
	a.now = func() time.Time { return now }

	a.bind("sess-1", "mockprov", "k1", "test-model")
	if _, ok := a.lookup("sess-1"); !ok {
		t.Fatal("fresh binding must resolve")
	}
	a.now = func() time.Time { return now.Add(2 * time.Hour) }
	if _, ok := a.lookup("sess-1"); ok {
		t.Fatal("expired binding must be dropped")
	}
	if a.size() != 0 {
		t.Fatalf("expired entry not deleted, size=%d", a.size())
	}
}

// TestAffinityHealthInvalidation: a bound key that cools down (or breaks)
// invalidates the binding (design §8 绑定失效条件).
func TestAffinityHealthInvalidation(t *testing.T) {
	store, _, _ := fixture(t, "http://127.0.0.1:1")
	rt := newRouter(store, DefaultCooldown)
	a := newSessionAffinity(rt, time.Hour)

	a.bind("sess-2", "mockprov", "k1", "test-model")
	if _, ok := a.lookup("sess-2"); !ok {
		t.Fatal("healthy binding must resolve")
	}
	rt.fail(candidate{providerID: "mockprov", keyID: "k1"})
	if _, ok := a.lookup("sess-2"); ok {
		t.Fatal("cooling target must invalidate the binding")
	}
	// Unknown model/key combinations are unhealthy too.
	a.bind("sess-3", "mockprov", "absent-key", "test-model")
	if _, ok := a.lookup("sess-3"); ok {
		t.Fatal("binding to a key that no longer serves the model must invalidate")
	}
}

// TestAffinityProviderPreference pins §8's 能换 key 解决的不换 provider:
// with a healthy binding, acquirePreferred puts the bound provider first
// even when round-robin would pick the other one.
func TestAffinityProviderPreference(t *testing.T) {
	store, _, _ := fixture(t, "http://127.0.0.1:1")
	// Second provider serving the same model, alphabetically BEFORE the
	// first so plain round-robin/rotation would favor it.
	if err := store.CreateProvider(provider.Provider{ID: "aprov", Name: "A", BaseURL: "http://127.0.0.1:1"}); err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}
	if err := store.CreateKey(provider.Key{ID: "ka", ProviderID: "aprov", SecretID: "ref-ka", Status: provider.KeyActive, Weight: 1}); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if err := store.CreateModel(provider.Model{ID: "aprov/ka/test", ProviderID: "aprov", KeyID: "ka", Name: "test-model", Upstream: "test-model"}); err != nil {
		t.Fatalf("CreateModel: %v", err)
	}
	rt := newRouter(store, DefaultCooldown)

	// Without preference: round-robin visits both providers.
	seen := map[string]int{}
	for i := 0; i < 4; i++ {
		c, ok := rt.pick("test-model")
		if !ok {
			t.Fatalf("pick %d failed", i)
		}
		seen[c.providerID]++
	}
	if len(seen) < 2 {
		t.Fatalf("plain rotation should visit both providers: %v", seen)
	}

	// With a binding, every pick lands on the preferred provider while it
	// stays healthy.
	for i := 0; i < 4; i++ {
		c, _, st := rt.acquirePreferred(context.Background(), "test-model", time.Nanosecond, "mockprov")
		if st != acquireOK {
			t.Fatalf("pick %d status %v", i, st)
		}
		if c.providerID != "mockprov" {
			t.Fatalf("pick %d ignored the bound provider: %s", i, c.providerID)
		}
		rt.success(c)
	}
}

// TestAffinityStickyEndToEnd: two auto requests under one session id bind
// to and stick with the same provider/key.
func TestAffinityStickyEndToEnd(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer upstream.Close()
	srv, _ := autoServer(t, upstream.URL)
	base := startProxy(t, srv)

	post := func(body string) int {
		req, _ := http.NewRequest(http.MethodPost, base+"/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("X-Claude-Code-Session-Id", "sticky-session-001")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	if got := post(`{"model":"auto","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`); got != 200 {
		t.Fatalf("status 1 = %d", got)
	}
	b1, ok := srv.affinity.lookup("sticky-session-001")
	if !ok {
		t.Fatal("first auto request did not bind the session")
	}
	if got := post(`{"model":"auto","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`); got != 200 {
		t.Fatalf("status 2 = %d", got)
	}
	b2, ok := srv.affinity.lookup("sticky-session-001")
	if !ok {
		t.Fatal("binding missing after second request")
	}
	if b1.providerID != b2.providerID || b1.keyID != b2.keyID || b1.model != b2.model {
		t.Fatalf("session drifted: %+v vs %+v", b1, b2)
	}
}

// TestAffinityRebindCostRecorded: a session bound to a routine-tier model
// that later sends a thinking request keeps the bound model (sticky wins
// over the deep upgrade) and records the cache rebind cost into usage.
func TestAffinityRebindCostRecorded(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer upstream.Close()
	srv, rec := autoServer(t, upstream.URL)
	base := startProxy(t, srv)

	// Hand-bind the session to the routine target (as a prior plain auto
	// request would have).
	srv.affinity.bind("rebind-session-01", "mockprov", "k1", "glm-5.3-flash")

	req, _ := http.NewRequest(http.MethodPost, base+"/v1/chat/completions",
		strings.NewReader(`{"model":"auto","thinking":{"type":"enabled"},"max_tokens":8000,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("X-Claude-Code-Session-Id", "rebind-session-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	rec1 := rec.last(t, 1)
	if rec1.CacheRebindCost == 0 {
		t.Fatalf("cache_rebind_cost not recorded: %+v", rec1)
	}
	if !strings.Contains(rec1.DecisionReason, "cache rebind") {
		t.Fatalf("reason: %q", rec1.DecisionReason)
	}
	if rec1.TaskTier != string(auto.TierDeep) {
		t.Fatalf("tier: %q", rec1.TaskTier)
	}
	b, ok := srv.affinity.lookup("rebind-session-01")
	if !ok || b.model != "glm-5.3-flash" {
		t.Fatalf("rebound binding: %+v ok=%v", b, ok)
	}
}
