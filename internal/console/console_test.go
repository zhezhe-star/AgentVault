package console

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhezhe-star/AgentVault/internal/app"
	"github.com/zhezhe-star/AgentVault/internal/provider"
	"github.com/zhezhe-star/AgentVault/internal/secret"
)

func fixture(t *testing.T) *provider.Store {
	t.Helper()
	store, err := provider.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	_ = store.CreateProvider(provider.Provider{ID: "bigmodel", Name: "Zhipu BigModel", BaseURL: "https://open.bigmodel.cn/api/paas/v4"})
	_ = store.CreateKey(provider.Key{ID: "primary", ProviderID: "bigmodel", SecretID: "ref", Status: provider.KeyActive, Weight: 1})
	_ = store.CreateModel(provider.Model{ID: "m1", ProviderID: "bigmodel", KeyID: "primary", Name: "glm-5.3", Upstream: "glm-5.3-flash"})
	return store
}

func TestOverviewAPIRealData(t *testing.T) {
	store := fixture(t)
	srv := httptest.NewServer(Handler(store, app.KeyRef))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/console/api/overview")
	if err != nil {
		t.Fatalf("get overview: %v", err)
	}
	defer resp.Body.Close()
	var out Overview
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Providers) != 1 {
		t.Fatalf("providers = %d, want 1", len(out.Providers))
	}
	p := out.Providers[0]
	if p.ID != "bigmodel" || len(p.Keys) != 1 || len(p.Keys[0].Models) != 1 {
		t.Fatalf("unexpected overview: %+v", p)
	}
	if p.Keys[0].Models[0].Name != "glm-5.3" {
		t.Fatalf("model name = %q", p.Keys[0].Models[0].Name)
	}
	if p.Keys[0].Ref != "ref:bigmodel/primary" {
		t.Fatalf("ref = %q", p.Keys[0].Ref)
	}
}

func TestUIPageServed(t *testing.T) {
	store := fixture(t)
	srv := httptest.NewServer(Handler(store, app.KeyRef))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("get /: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(body), "AgentVault") {
		t.Fatalf("index not served: status=%d", resp.StatusCode)
	}
}

// TestNoSecretInOverview pins the invariant: even with a secret in the
// backend, the console API must never carry key material.
func TestNoSecretInOverview(t *testing.T) {
	store := fixture(t)
	sec := secret.NewMemoryStore()
	_ = sec.Set("agentvault", "bigmodel/primary", "super-secret-key-material")
	srv := httptest.NewServer(Handler(store, app.KeyRef))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/console/api/overview")
	if err != nil {
		t.Fatalf("get overview: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), "super-secret-key-material") {
		t.Fatal("console API leaked secret")
	}
}
