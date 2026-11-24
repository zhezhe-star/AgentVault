package migrate

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhezhe-star/AgentVault/internal/provider"
	"github.com/zhezhe-star/AgentVault/internal/proxy"
	"github.com/zhezhe-star/AgentVault/internal/secret"
)

// TestProxyAttachRewritesBaseURLAndServes applies a plan with ProxyAddr and
// then drives one OpenAI-shaped call through the real local proxy — the
// "client semantics" the rewritten config promises.
func TestProxyAttachRewritesBaseURLAndServes(t *testing.T) {
	home := fixtureHome(t)
	found, err := NewScanner(home).Scan()
	if err != nil {
		t.Fatal(err)
	}
	plan := BuildPlan(found)
	store := secret.NewMemoryStore()
	rep, err := ApplyWithOptions(store, "agentvault", plan, Options{ProxyAddr: "127.0.0.1:8787"})
	if err != nil {
		t.Fatal(err)
	}

	// litellm api_base now points at the proxy.
	yml, _ := os.ReadFile(filepath.Join(home, ".config/litellm/config.yaml"))
	if !strings.Contains(string(yml), "api_base: http://127.0.0.1:8787/v1") {
		t.Fatalf("litellm api_base not rewritten:\n%s", yml)
	}
	// claude settings base_url follows its token sibling.
	settingsPath := filepath.Join(home, ".claude/settings.json")
	var doc struct {
		Env map[string]string `json:"env"`
	}
	b, _ := os.ReadFile(settingsPath)
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Env["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:8787" {
		t.Fatalf("ANTHROPIC_BASE_URL = %s", doc.Env["ANTHROPIC_BASE_URL"])
	}
	// No plaintext anywhere in rewritten files.
	for _, f := range rep.Files {
		b, _ := os.ReadFile(f)
		for _, s := range secretExpr() {
			if strings.Contains(string(b), s) {
				t.Fatalf("%s still contains plaintext", f)
			}
		}
	}

	// Real proxy on a free loopback port with one stored key wired to a mock
	// upstream; the rewritten-client semantics must complete a call.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ak-volc-abcdef1234567890abcdef" {
			http.Error(w, "bad key", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`))
	}))
	defer upstream.Close()

	pstore, err := provider.Open(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer pstore.Close()
	if err := pstore.CreateProvider(provider.Provider{ID: "ark", Name: "Volcengine Ark", BaseURL: upstream.URL}); err != nil {
		t.Fatal(err)
	}
	if err := pstore.CreateKey(provider.Key{ID: "ark_api_key", ProviderID: "ark", SecretID: "ark/ark_api_key", Status: provider.KeyActive}); err != nil {
		t.Fatal(err)
	}
	if err := pstore.CreateModel(provider.Model{ID: "ark/ark_api_key/ark-model", ProviderID: "ark", KeyID: "ark_api_key", Name: "ark-model", Upstream: "ark-model"}); err != nil {
		t.Fatal(err)
	}

	addr := freeLoopbackAddr(t)
	srv := proxy.New(pstore, store, "agentvault")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.ListenAndServe(ctx, addr) }()
	waitHealth(t, addr)

	body, _ := json.Marshal(map[string]any{"model": "ark-model", "messages": []map[string]string{{"role": "user", "content": "hi"}}})
	resp, err := http.Post("http://"+addr+"/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("proxy call status %d", resp.StatusCode)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Choices) != 1 || out.Choices[0].Message.Content != "ok" {
		t.Fatalf("unexpected proxy response: %+v", out)
	}
}

// freeLoopbackAddr reserves a free 127.0.0.1 port and releases it.
func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

func waitHealth(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := http.Get("http://" + addr + "/healthz"); err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("proxy did not come up in time")
}
