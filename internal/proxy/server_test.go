package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhezhe-star/AgentVault/internal/provider"
	"github.com/zhezhe-star/AgentVault/internal/secret"
	"github.com/zhezhe-star/AgentVault/internal/usage"
)

const testKey = "av-proxy-test-key-0123456789"

// fixture builds a store + secret backend with one provider/key/model whose
// upstream is the given mock server URL.
func fixture(t *testing.T, upstreamURL string) (*provider.Store, secret.Store, string) {
	t.Helper()
	store, err := provider.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	sec := secret.NewMemoryStore()

	if err := store.CreateProvider(provider.Provider{ID: "mockprov", Name: "Mock", BaseURL: upstreamURL}); err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}
	if err := store.CreateKey(provider.Key{ID: "k1", ProviderID: "mockprov", SecretID: "ref", Status: provider.KeyActive, Weight: 1}); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if err := sec.Set("agentvault", "mockprov/k1", testKey); err != nil {
		t.Fatalf("secret set: %v", err)
	}
	if err := store.CreateModel(provider.Model{ID: "m1", ProviderID: "mockprov", KeyID: "k1", Name: "test-model", Upstream: "test-model"}); err != nil {
		t.Fatalf("CreateModel: %v", err)
	}
	return store, sec, "agentvault"
}

// startProxy launches the server on a loopback port and returns its base URL.
func startProxy(t *testing.T, s *Server) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := newTestHTTPServer(s.recoverMiddleware(s.mux))
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return "http://" + ln.Addr().String()
}

func TestChatCompletionsForwarding(t *testing.T) {
	var gotAuth, gotPath string
	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		gotBody, _ = bufReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"chatcmpl-1","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"hi"}}]}`)
	}))
	defer upstream.Close()

	store, sec, svc := fixture(t, upstream.URL)
	srv := New(store, sec, svc)
	base := startProxy(t, srv)

	resp, err := http.Post(base+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"test-model","messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if gotAuth != "Bearer "+testKey {
		t.Fatalf("upstream auth = %q, want Bearer key", gotAuth)
	}
	if gotPath != "/chat/completions" {
		t.Fatalf("upstream path = %q", gotPath)
	}
	var body map[string]any
	if err := json.Unmarshal(gotBody, &body); err != nil || body["model"] != "test-model" {
		t.Fatalf("upstream body not forwarded: %v %s", err, gotBody)
	}
	// Response is OpenAI chat format.
	var chat map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&chat); err != nil || chat["object"] != "chat.completion" {
		t.Fatalf("response not OpenAI chat format: %v", err)
	}
}

func TestModelAliasRewrite(t *testing.T) {
	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = bufReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"chatcmpl-1","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"hi"}}]}`)
	}))
	defer upstream.Close()

	store, sec, svc := fixture(t, upstream.URL)
	// An alias row: clients ask for "alias-a", the provider only knows "test-model".
	if err := store.CreateModel(provider.Model{ID: "m2", ProviderID: "mockprov", KeyID: "k1", Name: "alias-a", Upstream: "test-model"}); err != nil {
		t.Fatalf("CreateModel alias: %v", err)
	}
	srv := New(store, sec, svc)
	base := startProxy(t, srv)

	resp, err := http.Post(base+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"alias-a","messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body map[string]any
	if err := json.Unmarshal(gotBody, &body); err != nil {
		t.Fatalf("upstream body: %v", err)
	}
	if body["model"] != "test-model" {
		t.Fatalf("upstream model = %v, want rewritten %q", body["model"], "test-model")
	}
	if _, ok := body["messages"]; !ok {
		t.Fatalf("non-model fields lost during rewrite: %s", gotBody)
	}
}

func TestLoopbackOnly(t *testing.T) {
	store, sec, svc := fixture(t, "http://127.0.0.1:1")
	srv := New(store, sec, svc)
	if err := srv.ListenAndServe(context.Background(), "0.0.0.0:0"); err == nil {
		t.Fatal("bound non-loopback address; must refuse")
	}
}

func TestPanicRecovered(t *testing.T) {
	store, sec, svc := fixture(t, "http://127.0.0.1:1")
	srv := New(store, sec, svc)

	// /healthz works; then simulate a panic by registering a panicking handler
	// through the mux is not possible post-construction, so instead verify
	// recoverMiddleware directly.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	srv.recoverMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	})).ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("recovered status = %d, want 500", rec.Code)
	}
}

func TestModelsEndpoint(t *testing.T) {
	store, sec, svc := fixture(t, "http://127.0.0.1:1")
	srv := New(store, sec, svc)
	base := startProxy(t, srv)

	resp, err := http.Get(base + "/v1/models")
	if err != nil {
		t.Fatalf("get models: %v", err)
	}
	defer resp.Body.Close()
	var out modelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Object != "list" || len(out.Data) != 1 || out.Data[0].ID != "test-model" {
		t.Fatalf("unexpected models payload: %+v", out)
	}
}

func TestUnknownModel(t *testing.T) {
	store, sec, svc := fixture(t, "http://127.0.0.1:1")
	srv := New(store, sec, svc)
	base := startProxy(t, srv)

	resp, err := http.Post(base+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"nope","messages":[]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	// Error body must not leak the secret (it never touched this path).
	var b bytes.Buffer
	_, _ = b.ReadFrom(resp.Body)
	if strings.Contains(b.String(), testKey) {
		t.Fatal("error body leaked key")
	}
}

// newTestHTTPServer isolates the http.Server construction from ListenAndServe
// (which enforces loopback) so tests can drive the mux directly.
func newTestHTTPServer(h http.Handler) *http.Server {
	return &http.Server{Handler: h}
}

// bufReadAll drains r into a byte slice.
func bufReadAll(r io.Reader) ([]byte, error) {
	return io.ReadAll(r)
}

// soloFixture is a one-key provider pointed at the mock upstream, plus a
// usage store so throttling records are observable.
func soloFixture(t *testing.T, upstreamURL string) (*Server, *usage.Store) {
	t.Helper()
	store, sec, svc := fixture(t, upstreamURL)
	ustore, err := usage.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatalf("usage open: %v", err)
	}
	t.Cleanup(func() { ustore.Close() })
	srv := New(store, sec, svc)
	srv.EnableUsage(ustore)
	srv.SetUsageSink(func(r usage.Record) { _ = ustore.Insert(r) })
	return srv, ustore
}

func postChat(t *testing.T, base, model string) *http.Response {
	t.Helper()
	resp, err := http.Post(base+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"`+model+`","messages":[]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	return resp
}

// Scenario 1: one key, upstream 429s. The first request attempts the key
// and relays the 429; the second finds every key cooling and must get 429
// with a Retry-After header (not 404), plus a usage record.
func TestAllKeysCoolingReturns429WithRetryAfter(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"message":"rate limited"}}`)
	}))
	defer upstream.Close()
	srv, ustore := soloFixture(t, upstream.URL)
	base := startProxy(t, srv)

	resp1 := postChat(t, base, "test-model")
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("first request status = %d, want relayed 429", resp1.StatusCode)
	}

	resp2 := postChat(t, base, "test-model")
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("all-cooling status = %d, want 429", resp2.StatusCode)
	}
	if ra := resp2.Header.Get("Retry-After"); ra == "" {
		t.Fatal("all-cooling 429 missing Retry-After header")
	}

	recs, err := ustore.Query(100)
	if err != nil {
		t.Fatalf("usage list: %v", err)
	}
	var cooling []usage.Record
	for _, r := range recs {
		if r.Status == http.StatusTooManyRequests && r.KeyID == "" {
			cooling = append(cooling, r)
		}
	}
	if len(cooling) == 0 {
		t.Fatalf("no all-cooling usage record; records: %+v", recs)
	}
}

// Scenario 2: a plain 429 (no Retry-After, no quota wording) is concurrency
// throttling and takes the short cooldown, not the 60s default.
func TestConcurrency429UsesShortCooldown(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"message":"rate limited"}}`)
	}))
	defer upstream.Close()
	store, sec, svc := fixture(t, upstream.URL)
	srv := New(store, sec, svc)
	base := startProxy(t, srv)

	resp := postChat(t, base, "test-model")
	resp.Body.Close()

	ra := srv.router.retryAfter()
	if ra <= 0 || ra > DefaultShortCooldown {
		t.Fatalf("retryAfter = %v, want short cooldown (0, %v]", ra, DefaultShortCooldown)
	}
}

// Scenario 3: an upstream Retry-After header sets the cooldown to its value.
func TestRetryAfterHeaderSetsCooldown(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"message":"slow down"}}`)
	}))
	defer upstream.Close()
	store, sec, svc := fixture(t, upstream.URL)
	srv := New(store, sec, svc)
	base := startProxy(t, srv)

	resp := postChat(t, base, "test-model")
	resp.Body.Close()

	// Ceil to seconds: 7s remaining minus a hair of test latency stays 7.
	if ra := srv.router.retryAfter(); ra != 7*time.Second {
		t.Fatalf("retryAfter = %v, want 7s from upstream Retry-After", ra)
	}
}

// Scenario 4: quota-worded 429s keep the long cooldown, and sustained 429s
// still escalate to the breaker (unchanged behavior).
func TestQuota429LongCooldownAndBreakerEscalation(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"message":"insufficient quota for this key"}}`)
	}))
	defer upstream.Close()
	store, sec, svc := fixture(t, upstream.URL)
	srv := New(store, sec, svc)
	base := startProxy(t, srv)

	resp := postChat(t, base, "test-model")
	resp.Body.Close()
	if ra := srv.router.retryAfter(); ra <= DefaultShortCooldown {
		t.Fatalf("quota 429 retryAfter = %v, want long cooldown", ra)
	}

	// Keep hitting it: with the cooldown shrunk to near zero every request
	// reaches the upstream again, so consecutive 429s escalate to the breaker.
	srv.router.cooldown = time.Millisecond
	// Clear the long cooldown left by the first request so the loop's
	// requests actually reach the upstream again.
	srv.router.mu.Lock()
	srv.router.cooldowns = map[string]time.Time{}
	srv.router.fails = map[string]int{}
	srv.router.mu.Unlock()
	for i := 0; i < BreakerThreshold; i++ {
		resp := postChat(t, base, "test-model")
		resp.Body.Close()
		time.Sleep(5 * time.Millisecond) // let the 1ms cooldown expire
	}
	if srv.router.breakerCount() != 1 {
		t.Fatalf("breaker count = %d, want 1 after %d quota 429s", srv.router.breakerCount(), BreakerThreshold)
	}
}

// The Responses bridge path answers an all-cooling request the same way:
// 429 + Retry-After plus a usage record (issue #5 hit this path via glm).
func TestResponsesAllKeysCoolingReturns429(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"message":"rate limited"}}`)
	}))
	defer upstream.Close()
	srv, ustore := soloFixture(t, upstream.URL)
	base := startProxy(t, srv)

	resp1, err := http.Post(base+"/v1/responses", "application/json",
		strings.NewReader(`{"model":"test-model","input":"hi"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp1.Body.Close()

	resp2, err := http.Post(base+"/v1/responses", "application/json",
		strings.NewReader(`{"model":"test-model","input":"hi"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("all-cooling status = %d, want 429", resp2.StatusCode)
	}
	if resp2.Header.Get("Retry-After") == "" {
		t.Fatal("missing Retry-After header")
	}
	recs, err := ustore.Query(100)
	if err != nil {
		t.Fatalf("usage query: %v", err)
	}
	for _, r := range recs {
		if r.Status == http.StatusTooManyRequests && r.KeyID == "" {
			return // cooling record present
		}
	}
	t.Fatalf("no all-cooling usage record; records: %+v", recs)
}
