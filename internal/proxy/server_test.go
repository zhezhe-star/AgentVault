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

	"github.com/zhezhe-star/AgentVault/internal/provider"
	"github.com/zhezhe-star/AgentVault/internal/secret"
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
