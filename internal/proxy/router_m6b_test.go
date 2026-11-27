package proxy

// M6-B router regression + new-path tests (design §10):
// - per-candidate alias rewrite (item 2): the picked provider's own
//   registered upstream id, never a first match across the models table.
// - auto re-decision (item 3): all-cooling target falls back per
//   auto.ReDecide without breaking known()'s 404-vs-429 semantics.
// - gate review (item 4): auto requests occupy the same account-level
//   concurrency gate as explicit aliases.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhezhe-star/AgentVault/internal/provider"
)

// TestAliasRewritePerCandidate pins §10 item 2: one alias registered under
// two providers with DIFFERENT upstream ids must rewrite to each picked
// provider's own id — the old pre-pick rewrite keyed on the alias name
// alone and sent the first match's id to both.
func TestAliasRewritePerCandidate(t *testing.T) {
	var mu sync.Mutex
	var seen []string // every body model id the upstream saw
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		model, _ := body["model"].(string)
		mu.Lock()
		seen = append(seen, model)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"c1","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer upstream.Close()

	store, sec, svc := fixture(t, upstream.URL)
	// A second provider pointing at the same upstream host with a
	// different upstream id for the same alias.
	if err := store.CreateProvider(provider.Provider{ID: "secondprov", Name: "Second", BaseURL: upstream.URL}); err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}
	if err := store.CreateKey(provider.Key{ID: "k2", ProviderID: "secondprov", SecretID: "ref2", Status: provider.KeyActive, Weight: 1}); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if err := sec.Set("agentvault", "secondprov/k2", testKey); err != nil {
		t.Fatalf("secret set: %v", err)
	}
	if err := store.CreateModel(provider.Model{ID: "secondprov/k2/duo", ProviderID: "secondprov", KeyID: "k2", Name: "duo-alias", Upstream: "secondprov-upstream-id"}); err != nil {
		t.Fatalf("CreateModel: %v", err)
	}
	// The base fixture's mockprov serves the same alias under a different
	// upstream id.
	if err := store.CreateModel(provider.Model{ID: "mockprov/k1/duo", ProviderID: "mockprov", KeyID: "k1", Name: "duo-alias", Upstream: "mockprov-upstream-id"}); err != nil {
		t.Fatalf("CreateModel: %v", err)
	}

	srv := New(store, sec, svc)
	base := startProxy(t, srv)
	for i := 0; i < 4; i++ {
		resp, err := http.Post(base+"/v1/chat/completions", "application/json",
			strings.NewReader(`{"model":"duo-alias","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("call %d status %d", i, resp.StatusCode)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	found := map[string]int{}
	for _, model := range seen {
		switch model {
		case "mockprov-upstream-id", "secondprov-upstream-id":
			found[model]++
		default:
			t.Fatalf("upstream saw unrewritten model %q", model)
		}
	}
	if found["mockprov-upstream-id"] == 0 || found["secondprov-upstream-id"] == 0 {
		t.Fatalf("expected each provider to see its own upstream id, got %v", found)
	}
}

// TestAutoRedecideOnCooldown covers §10 item 3 end to end: the routine
// target's only key is cooling down, so the auto path re-decides to the
// other in-plan alias and serves the request instead of answering 429.
func TestAutoRedecideOnCooldown(t *testing.T) {
	var seen []string
	upstream := upstreamEcho(t, &seen)
	store, sec, svc := fixture(t, upstream.URL)
	// glm-5.4 and glm-5.4[1m] on the healthy key; glm-5.3-flash isolated
	// on a second provider whose key we cool down.
	for _, m := range []provider.Model{
		{ID: "mockprov/k1/deep", ProviderID: "mockprov", KeyID: "k1", Name: "glm-5.4", Upstream: "glm-5.4"},
		{ID: "mockprov/k1/1m", ProviderID: "mockprov", KeyID: "k1", Name: "glm-5.4[1m]", Upstream: "glm-5.4[1m]"},
	} {
		if err := store.CreateModel(m); err != nil {
			t.Fatalf("CreateModel %s: %v", m.Name, err)
		}
	}
	if err := store.CreateProvider(provider.Provider{ID: "flashprov", Name: "Flash", BaseURL: upstream.URL}); err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}
	if err := store.CreateKey(provider.Key{ID: "kf", ProviderID: "flashprov", SecretID: "reff", Status: provider.KeyActive, Weight: 1}); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if err := store.CreateModel(provider.Model{ID: "flashprov/kf/flash", ProviderID: "flashprov", KeyID: "kf", Name: "glm-5.3-flash", Upstream: "glm-5.3-flash"}); err != nil {
		t.Fatalf("CreateModel: %v", err)
	}

	srv := New(store, sec, svc)
	rec := &usageRecorder{}
	srv.SetUsageSink(rec.add)
	// Cool the flash alias's only key.
	cands, err := srv.router.candidates("glm-5.3-flash")
	if err != nil || len(cands) == 0 {
		t.Fatalf("flash candidates: %v", err)
	}
	for _, c := range cands {
		srv.router.fail(c, withCooldown(time.Minute))
	}
	// The cooldown snapshot (§10 item 1) must see it.
	if st := srv.router.cooldownState("glm-5.3-flash"); !st.allCooling {
		t.Fatalf("cooldownState: %+v", st)
	}

	base := startProxy(t, srv)
	resp, err := http.Post(base+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"auto","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want re-decided 200", resp.StatusCode)
	}
	deadline := time.Now().Add(2 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		if len(seen) > 0 {
			last = seen[len(seen)-1]
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if last != "glm-5.4[1m]" {
		t.Fatalf("upstream saw %q, want the re-decided target", last)
	}
	rec1 := rec.last(t, 1)
	if !strings.Contains(rec1.DecisionReason, "re-decided") {
		t.Fatalf("reason: %q", rec1.DecisionReason)
	}
}

// TestAutoDeepNoRedecideDowngrade: deep's target all-cooling must stay a
// 429 (blacklist — never re-route), preserving known()'s semantics.
func TestAutoDeepNoRedecideDowngrade(t *testing.T) {
	var seen []string
	upstream := upstreamEcho(t, &seen)
	srv, _ := autoServer(t, upstream.URL)

	// Cool every key serving glm-5.4 (the deep target).
	cands, err := srv.router.candidates("glm-5.4")
	if err != nil || len(cands) == 0 {
		t.Fatalf("deep candidates: %v", err)
	}
	for _, c := range cands {
		srv.router.fail(c, withCooldown(time.Minute))
	}

	base := startProxy(t, srv)
	resp, err := http.Post(base+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"auto-deep","max_tokens":8000,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (deep never downgrades)", resp.StatusCode)
	}
	if len(seen) != 0 {
		t.Fatalf("upstream must not be contacted, saw %v", seen)
	}
}

// TestGateNotBypassedByAuto (§10 item 4 review): an auto request occupies
// the same account-level gate as explicit aliases — with the provider at
// its ceiling, the auto request queues (in-flight 1, queued 1) rather
// than bypassing the gate.
func TestGateNotBypassedByAuto(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"c1","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer upstream.Close()
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })

	srv, _ := autoServer(t, upstream.URL)
	if err := srv.store.SetConcurrency("mockprov", 1); err != nil {
		t.Fatalf("SetConcurrency: %v", err)
	}
	base := startProxy(t, srv)

	go func() {
		resp, err := http.Post(base+"/v1/chat/completions", "application/json",
			strings.NewReader(`{"model":"glm-5.4","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
		if err == nil {
			resp.Body.Close()
		}
	}()
	// Wait for the first request to hold the single slot.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if srv.router.gateStats()["mockprov"].InFlight == 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}

	autoDone := make(chan int, 1)
	go func() {
		resp, err := http.Post(base+"/v1/chat/completions", "application/json",
			strings.NewReader(`{"model":"auto","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			autoDone <- -1
			return
		}
		defer resp.Body.Close()
		autoDone <- resp.StatusCode
	}()
	time.Sleep(50 * time.Millisecond)
	stats := srv.router.gateStats()["mockprov"]
	if stats.InFlight != 1 || stats.Queued != 1 {
		t.Fatalf("auto bypassed the gate: %+v", stats)
	}
	releaseOnce.Do(func() { close(release) })
	if st := <-autoDone; st != 200 {
		t.Fatalf("queued auto request status %d", st)
	}
}
