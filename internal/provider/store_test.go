package provider

import (
	"errors"
	"path/filepath"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestMigrateIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	s1, err := Open(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	s1.Close()
	// Reopening the same file re-runs Open/migrate without error.
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer s2.Close()
	var current int
	if err := s2.db.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&current); err != nil {
		t.Fatalf("read version: %v", err)
	}
	if current != len(migrations) {
		t.Fatalf("version = %d, want %d", current, len(migrations))
	}
}

func TestProviderLifecycle(t *testing.T) {
	s := openTestStore(t)

	if err := s.CreateProvider(Provider{ID: "bigmodel", Name: "Zhipu BigModel", BaseURL: "https://open.bigmodel.cn/api/paas/v4"}); err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}
	got, err := s.GetProvider("bigmodel")
	if err != nil {
		t.Fatalf("GetProvider: %v", err)
	}
	if got.Name != "Zhipu BigModel" || got.BaseURL == "" {
		t.Fatalf("unexpected provider: %+v", got)
	}

	if _, err := s.GetProvider("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetProvider(missing) err = %v, want ErrNotFound", err)
	}

	ps, err := s.ListProviders()
	if err != nil || len(ps) != 1 {
		t.Fatalf("ListProviders: %v len=%d", err, len(ps))
	}
}

func TestKeyStatusTransitions(t *testing.T) {
	s := openTestStore(t)
	_ = s.CreateProvider(Provider{ID: "p1", Name: "P1", BaseURL: "https://example.com"})

	k := Key{ID: "k1", ProviderID: "p1", SecretID: "ref-1", Status: KeyActive, Weight: 1}
	if err := s.CreateKey(k); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}

	keys, err := s.ListKeys("p1")
	if err != nil || len(keys) != 1 || keys[0].Status != KeyActive {
		t.Fatalf("ListKeys: %v %+v", err, keys)
	}

	for _, step := range []struct{ to string }{{KeyDisabled}, {KeyRevoked}} {
		if err := s.SetKeyStatus("k1", step.to); err != nil {
			t.Fatalf("SetKeyStatus(%s): %v", step.to, err)
		}
	}
	keys, _ = s.ListKeys("p1")
	if keys[0].Status != KeyRevoked {
		t.Fatalf("status = %s, want revoked", keys[0].Status)
	}

	// Revoked keys disappear from the routing view.
	active, err := s.ListActiveKeys()
	if err != nil || len(active) != 0 {
		t.Fatalf("ListActiveKeys: %v len=%d", err, len(active))
	}

	if err := s.SetKeyStatus("missing", KeyDisabled); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetKeyStatus(missing) err = %v, want ErrNotFound", err)
	}
}

func TestModelMappings(t *testing.T) {
	s := openTestStore(t)
	_ = s.CreateProvider(Provider{ID: "p1", Name: "P1", BaseURL: "https://example.com"})
	_ = s.CreateKey(Key{ID: "k1", ProviderID: "p1", SecretID: "ref-1", Status: KeyActive, Weight: 1})

	_ = s.CreateModel(Model{ID: "m1", ProviderID: "p1", KeyID: "k1", Name: "glm-5.3", Upstream: "glm-5.3-flash"})
	_ = s.CreateModel(Model{ID: "m2", ProviderID: "p1", KeyID: "k1", Name: "glm-5.3-flash", Upstream: "glm-5.3-flash"})

	ms, err := s.ListModels("p1")
	if err != nil || len(ms) != 2 {
		t.Fatalf("ListModels: %v len=%d", err, len(ms))
	}
	all, err := s.ListAllModels()
	if err != nil || len(all) != 2 {
		t.Fatalf("ListAllModels: %v len=%d", err, len(all))
	}

	// Duplicate (key, name) must be rejected by the UNIQUE constraint.
	if err := s.CreateModel(Model{ID: "m3", ProviderID: "p1", KeyID: "k1", Name: "glm-5.3", Upstream: "x"}); err == nil {
		t.Fatal("duplicate model accepted, want UNIQUE violation")
	}
}

func TestConcurrencyColumn(t *testing.T) {
	s := openTestStore(t)
	// Migration 6 defaults rows without an explicit value (i.e. providers
	// that predate the column) to the measured GLM account ceiling of 3;
	// an explicit 0 records "unlimited".
	_ = s.CreateProvider(Provider{ID: "p1", Name: "P1", BaseURL: "https://example.com", Concurrency: 3})
	_ = s.CreateProvider(Provider{ID: "p2", Name: "P2", BaseURL: "https://example.com", Concurrency: 0})
	// A row written as the pre-M5 schema would have written it (no
	// concurrency column) must pick up the migration default of 3.
	if _, err := s.db.Exec(`INSERT INTO providers (id, name, base_url, created_at) VALUES ('legacy','L','https://example.com','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("legacy insert: %v", err)
	}

	p1, err := s.GetProvider("p1")
	if err != nil {
		t.Fatalf("GetProvider p1: %v", err)
	}
	if p1.Concurrency != 3 {
		t.Fatalf("explicit concurrency = %d, want 3", p1.Concurrency)
	}
	p2, _ := s.GetProvider("p2")
	if p2.Concurrency != 0 {
		t.Fatalf("explicit unlimited = %d, want 0", p2.Concurrency)
	}
	legacy, _ := s.GetProvider("legacy")
	if legacy.Concurrency != 3 {
		t.Fatalf("legacy default concurrency = %d, want 3", legacy.Concurrency)
	}

	if err := s.SetConcurrency("p1", 5); err != nil {
		t.Fatalf("SetConcurrency: %v", err)
	}
	if p, _ := s.GetProvider("p1"); p.Concurrency != 5 {
		t.Fatalf("after set concurrency = %d, want 5", p.Concurrency)
	}
	if err := s.SetConcurrency("missing", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetConcurrency(missing) err = %v, want ErrNotFound", err)
	}
}

func TestPlanLifecycle(t *testing.T) {
	s := openTestStore(t)
	if err := s.SetPlan(Plan{
		Provider: "bigmodel", PlanTier: "max", WindowType: "weekly",
		Limit: 100, Remaining: 91,
	}); err != nil {
		t.Fatalf("set plan: %v", err)
	}
	// Upsert overwrites the row in place (append-only table semantics).
	if err := s.SetPlan(Plan{
		Provider: "bigmodel", PlanTier: "max", WindowType: "weekly",
		Limit: 120, Remaining: 30,
	}); err != nil {
		t.Fatalf("upsert plan: %v", err)
	}
	p, err := s.GetPlan("bigmodel", "max", "weekly")
	if err != nil {
		t.Fatalf("get plan: %v", err)
	}
	if p.Limit != 120 || p.Remaining != 30 || p.Provider != "bigmodel" {
		t.Fatalf("plan row: %+v", p)
	}
	if p.RefreshedAt.IsZero() || p.VerifiedAt.IsZero() {
		t.Fatalf("refreshed/verified must default to now: %+v", p)
	}
	if _, err := s.GetPlan("bigmodel", "max", "5h"); err != ErrNotFound {
		t.Fatalf("missing plan should be ErrNotFound, got %v", err)
	}
	// Multiple windows per tier coexist.
	if err := s.SetPlan(Plan{Provider: "bigmodel", PlanTier: "max", WindowType: "5h", Limit: 10, Remaining: 10}); err != nil {
		t.Fatalf("set 5h plan: %v", err)
	}
	ps, err := s.ListPlans()
	if err != nil || len(ps) != 2 {
		t.Fatalf("list plans: %v %+v", err, ps)
	}
}
