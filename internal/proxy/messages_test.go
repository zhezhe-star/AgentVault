package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zhezhe-star/AgentVault/internal/provider"
	"github.com/zhezhe-star/AgentVault/internal/secret"
	"github.com/zhezhe-star/AgentVault/internal/usage"
)

// fixtureAnthropic adds an Anthropic-compatible provider pointing at the
// given upstream URL (registered under a distinct provider id).
func fixtureAnthropic(t *testing.T, upstreamURL string) (*provider.Store, secret.Store, string) {
	t.Helper()
	store, sec, svc := fixture(t, "http://127.0.0.1:1")
	if err := store.CreateProvider(provider.Provider{
		ID: "mockanthropic", Name: "MockAnthropic",
		BaseURL: "http://127.0.0.1:1", AnthropicBaseURL: upstreamURL,
	}); err != nil {
		t.Fatalf("CreateProvider anthropic: %v", err)
	}
	if err := store.CreateKey(provider.Key{ID: "ka", ProviderID: "mockanthropic", SecretID: "ref-a", Status: provider.KeyActive, Weight: 1}); err != nil {
		t.Fatalf("CreateKey anthropic: %v", err)
	}
	if err := sec.Set("agentvault", "mockanthropic/ka", testKey); err != nil {
		t.Fatalf("secret set: %v", err)
	}
	if err := store.CreateModel(provider.Model{ID: "ma", ProviderID: "mockanthropic", KeyID: "ka", Name: "anthro-model", Upstream: "glm-4.7"}); err != nil {
		t.Fatalf("CreateModel anthropic: %v", err)
	}
	return store, sec, svc
}

func TestMessagesPassthrough(t *testing.T) {
	var gotKey, gotVersion, gotPath string
	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		gotPath = r.URL.Path
		gotBody, _ = bufReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn"}`))
	}))
	defer upstream.Close()

	store, sec, svc := fixtureAnthropic(t, upstream.URL)
	base := startProxy(t, New(store, sec, svc))

	req, _ := http.NewRequest(http.MethodPost, base+"/v1/messages",
		strings.NewReader(`{"model":"anthro-model","max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post messages: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if gotPath != "/v1/messages" {
		t.Fatalf("upstream path = %q, want /v1/messages", gotPath)
	}
	if gotKey != testKey {
		t.Fatalf("upstream x-api-key = %q, want key", gotKey)
	}
	if gotVersion != "2023-06-01" {
		t.Fatalf("anthropic-version not passed through: %q", gotVersion)
	}
	// The alias is rewritten to the registered upstream model id before
	// forwarding; every other field passes through untouched.
	if !strings.Contains(string(gotBody), `"glm-4.7"`) || strings.Contains(string(gotBody), `"anthro-model"`) {
		t.Fatalf("body model not rewritten: %s", gotBody)
	}
	// Response body passes through untouched (Anthropic message shape).
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	if !strings.Contains(string(buf[:n]), `"type":"message"`) {
		t.Fatalf("response not Anthropic shape: %s", buf[:n])
	}
}

func TestMessagesOpenAIOnlyProviderRejected(t *testing.T) {
	// The base fixture's provider has no anthropic_base_url, so a
	// /v1/messages call to its model must 404 without leaking anything.
	store, sec, svc := fixture(t, "http://127.0.0.1:1")
	base := startProxy(t, New(store, sec, svc))

	resp, err := http.Post(base+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"test-model","max_tokens":1,"messages":[]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	if strings.Contains(string(buf[:n]), testKey) {
		t.Fatal("error body leaked key")
	}
}

func TestMessagesFailover(t *testing.T) {
	var hits int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		if hits == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":"rate limited"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"type":"message"}`))
	}))
	defer upstream.Close()

	store, sec, svc := fixtureAnthropic(t, upstream.URL)
	// Two keys on the anthropic provider; first one cools down, second serves.
	if err := store.CreateKey(provider.Key{ID: "kb", ProviderID: "mockanthropic", SecretID: "ref-b", Status: provider.KeyActive, Weight: 1}); err != nil {
		t.Fatalf("CreateKey kb: %v", err)
	}
	if err := sec.Set("agentvault", "mockanthropic/kb", testKey); err != nil {
		t.Fatalf("secret set kb: %v", err)
	}

	base := startProxy(t, New(store, sec, svc))
	resp, err := http.Post(base+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"anthro-model","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || hits != 2 {
		t.Fatalf("failover failed: status=%d hits=%d", resp.StatusCode, hits)
	}
}

// TestConsoleMountedAtRoot pins the mounting fix: the embedded UI must be
// reachable at / and the overview API at /console/api/overview through the
// proxy mux (the /console/ prefix mount used to 404).
func TestConsoleMountedAtRoot(t *testing.T) {
	store, sec, svc := fixture(t, "http://127.0.0.1:1")
	base := startProxy(t, New(store, sec, svc))

	resp, err := http.Get(base + "/")
	if err != nil {
		t.Fatalf("get /: %v", err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 8192)
	n, _ := resp.Body.Read(buf)
	if resp.StatusCode != 200 || !strings.Contains(string(buf[:n]), "AgentVault") {
		t.Fatalf("/ did not serve console UI: status=%d body=%q", resp.StatusCode, buf[:n])
	}

	resp2, err := http.Get(base + "/console/api/overview")
	if err != nil {
		t.Fatalf("get overview: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("overview status = %d, want 200", resp2.StatusCode)
	}
}

// TestUsageRecordedPerCall pins M3-1: one successful /v1/messages call
// produces exactly one usage record with real token counts parsed from the
// upstream response.
func TestUsageRecordedPerCall(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"type":"message","usage":{"input_tokens":12,"output_tokens":34}}`))
	}))
	defer upstream.Close()

	store, sec, svc := fixtureAnthropic(t, upstream.URL)
	srv := New(store, sec, svc)
	var records []usage.Record
	srv.SetUsageSink(func(r usage.Record) { records = append(records, r) })
	base := startProxy(t, srv)

	resp, err := http.Post(base+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"anthro-model","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	// The sink runs after the response is fully flushed, so poll briefly.
	deadline := time.Now().Add(2 * time.Second)
	for len(records) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if len(records) != 1 {
		t.Fatalf("usage records = %d, want 1", len(records))
	}
	r := records[0]
	if r.ProviderID != "mockanthropic" || r.KeyID != "ka" || r.Model != "anthro-model" ||
		r.Protocol != "anthropic" || r.Status != 200 || r.TokensIn != 12 || r.TokensOut != 34 || r.LatencyMS < 0 {
		t.Fatalf("unexpected usage record: %+v", r)
	}
}
