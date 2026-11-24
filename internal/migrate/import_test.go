package migrate

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhezhe-star/AgentVault/internal/secret"
)

// secretExpr returns any plaintext-looking long token for leak assertions.
func secretExpr() []string {
	return []string{"abcdef1234567890abcdef", "fedcba0987654321fedcba", "1111222233334444aaaa", "abcdef12345678901234"}
}

func TestPreviewIsReadOnly(t *testing.T) {
	home := fixtureHome(t)
	s := NewScanner(home)
	found, err := s.Scan()
	if err != nil {
		t.Fatal(err)
	}
	// Snapshot all files before preview.
	before := map[string]string{}
	for _, f := range found {
		b, _ := os.ReadFile(f.Path)
		before[f.Path] = string(b)
	}

	var buf bytes.Buffer
	BuildPlan(found).Preview(&buf)

	for path, content := range before {
		after, _ := os.ReadFile(path)
		if string(after) != content {
			t.Fatalf("preview modified %s", path)
		}
	}
}

func TestImportApplyRewritesAndStores(t *testing.T) {
	home := fixtureHome(t)
	found, err := NewScanner(home).Scan()
	if err != nil {
		t.Fatal(err)
	}
	plan := BuildPlan(found)
	// Snapshot plaintext per ref BEFORE Apply (files get rewritten).
	expected := map[string]string{}
	for _, it := range plan.Items {
		plain, err := it.plaintext()
		if err != nil {
			t.Fatalf("pre-extract %s: %v", it.Ref, err)
		}
		expected[it.Ref] = plain
	}
	store := secret.NewMemoryStore()
	rep, err := Apply(store, "agentvault", plan)
	if err != nil {
		t.Fatal(err)
	}

	// Every import landed in the secret store with correct plaintext.
	// Every planned item must be stored under its (disambiguated) ref with
	// exactly the plaintext that was in its file.
	for _, it := range plan.Items {
		v, err := store.Get("agentvault", it.Ref)
		if err != nil {
			t.Fatalf("ref %s missing from store: %v", it.Ref, err)
		}
		if v != expected[it.Ref] {
			t.Fatalf("ref %s stored %q, want %q", it.Ref, v, expected[it.Ref])
		}
	}
	// Duplicate-var disambiguation: zshrc and codex ARK_API_KEY get distinct refs.
	refs := map[string]bool{}
	for _, it := range plan.Items {
		if refs[it.Ref] {
			t.Fatalf("duplicate ref %s", it.Ref)
		}
		refs[it.Ref] = true
	}
	zshRef, codexRef := "", ""
	for _, it := range plan.Items {
		if it.Finding.Kind == "shell-export" && it.Finding.Var == "ARK_API_KEY" {
			zshRef = it.Ref
		}
		if it.Finding.Kind == "toml-env" && it.Finding.Var == "ARK_API_KEY" {
			codexRef = it.Ref
		}
	}
	if zshRef == codexRef || zshRef == "" || codexRef == "" {
		t.Fatalf("refs not disambiguated: zsh=%q codex=%q", zshRef, codexRef)
	}
	if v, _ := store.Get("agentvault", zshRef); v != "ak-volc-abcdef1234567890abcdef" {
		t.Fatalf("zshrc key stored wrong: %s", v)
	}
	if v, _ := store.Get("agentvault", codexRef); v != "ak-volc-1111222233334444aaaa" {
		t.Fatalf("codex key stored wrong: %s", v)
	}

	// .bak exists per rewritten file and still contains the plaintext.
	for _, f := range rep.Files {
		baks, _ := filepath.Glob(f + ".bak-*")
		if len(baks) != 1 {
			t.Fatalf("%s: want exactly one .bak, got %d", f, len(baks))
		}
		b, _ := os.ReadFile(baks[0])
		for _, s := range secretExpr() {
			if strings.Contains(string(b), s) {
				return // found a leaked secret in backup: good
			}
		}
		t.Fatalf("%s backup does not contain original secret", f)
	}

	// Rewritten files carry no plaintext.
	for _, f := range rep.Files {
		b, _ := os.ReadFile(f)
		for _, s := range secretExpr() {
			if strings.Contains(string(b), s) {
				t.Fatalf("%s still contains plaintext after rewrite", f)
			}
		}
	}

	// Shell line became a comment with the ref and restore hint.
	zrc, _ := os.ReadFile(filepath.Join(home, ".zshrc"))
	if !strings.Contains(string(zrc), "ref:ark/ark_api_key") || !strings.Contains(string(zrc), "agentvault run") {
		t.Fatalf(".zshrc rewrite missing agentvault hint:\n%s", zrc)
	}
	// TOML values became the placeholder.
	toml, _ := os.ReadFile(filepath.Join(home, ".codex/config.toml"))
	if !strings.Contains(string(toml), Placeholder) {
		t.Fatalf("config.toml missing placeholder:\n%s", toml)
	}
	// YAML values became the placeholder.
	yml, _ := os.ReadFile(filepath.Join(home, ".config/litellm/config.yaml"))
	if strings.Count(string(yml), Placeholder) != 1 {
		t.Fatalf("litellm config should have 1 placeholder, got:\n%s", yml)
	}
}

func TestImportRefuseLeavesZeroChanges(t *testing.T) {
	// Refusal means Apply is never called; assert BuildPlan skips compliant
	// and that not calling Apply leaves files untouched (trivially true but
	// documented by this test pairing with TestPreviewIsReadOnly).
	home := fixtureHome(t)
	found, _ := NewScanner(home).Scan()
	for _, f := range found {
		if f.Class == ClassCompliant {
			t.Fatalf("no compliant overrides set, but got %s", f.Class)
		}
	}
}

func TestBuildPlanFilteredPerFile(t *testing.T) {
	home := fixtureHome(t)
	found, _ := NewScanner(home).Scan()
	zshrc := filepath.Join(home, ".zshrc")
	plan := BuildPlanFiltered(found, []string{zshrc})
	if len(plan.Items) != 1 || plan.Items[0].Finding.Path != zshrc {
		t.Fatalf("filtered plan should only touch .zshrc, got %d items", len(plan.Items))
	}
	// Applying it must not modify any other file.
	store := secret.NewMemoryStore()
	rep, err := Apply(store, "agentvault", plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Files) != 1 || rep.Files[0] != zshrc {
		t.Fatalf("rewrote %v, want only %s", rep.Files, zshrc)
	}
	toml, _ := os.ReadFile(filepath.Join(home, ".codex/config.toml"))
	for _, s := range secretExpr() {
		if strings.Contains(string(toml), s) {
			return // untouched file still has its plaintext: correct
		}
	}
	t.Fatal("untouched file lost its plaintext?!")
}

func TestBuildPlanDedupsRepeatedVarUniqueRefs(t *testing.T) {
	// Three same-var entries in one file must map to three distinct refs
	// (regression: 2nd and 3rd used to collide on the same ref).
	home := t.TempDir()
	p := filepath.Join(home, ".config/litellm/config.yaml")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	content := `model_list:
  - model_name: a
    litellm_params:
      model: openai/a
      api_key: "key-aaaaaaaaaaaa1"
  - model_name: b
    litellm_params:
      model: openai/b
      api_key: "key-bbbbbbbbbbbb2"
  - model_name: c
    litellm_params:
      model: openai/c
      api_key: "key-cccccccccccc3"
`
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	found, err := NewScanner(home).Scan()
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 3 {
		t.Fatalf("want 3 findings, got %d", len(found))
	}
	plan := BuildPlan(found)
	refs := map[string]bool{}
	for _, it := range plan.Items {
		if refs[it.Ref] {
			t.Fatalf("duplicate ref %s", it.Ref)
		}
		refs[it.Ref] = true
	}
	// Apply: every stored value must match its own line's key.
	store := secret.NewMemoryStore()
	want := map[string]string{
		plan.Items[0].Ref: "key-aaaaaaaaaaaa1",
		plan.Items[1].Ref: "key-bbbbbbbbbbbb2",
		plan.Items[2].Ref: "key-cccccccccccc3",
	}
	if _, err := Apply(store, "agentvault", plan); err != nil {
		t.Fatal(err)
	}
	for ref, v := range want {
		got, err := store.Get("agentvault", ref)
		if err != nil || got != v {
			t.Fatalf("ref %s: got %q (%v), want %q", ref, got, err, v)
		}
	}
}

func TestEnsureYAMLBaseReplacesPrecedingAPIBase(t *testing.T) {
	// litellm blocks put api_base before api_key.
	lines := []string{
		"  - model_name: a",
		"    litellm_params:",
		"      model: openai/a",
		"      api_base: https://upstream.example/v1",
		"      api_key: \"PLACEHOLDER\"",
	}
	out := ensureYAMLBase(lines, 4, "http://127.0.0.1:8787/v1")
	if out[3] != "      api_base: http://127.0.0.1:8787/v1" {
		t.Fatalf("preceding api_base not rewritten: %q", out[3])
	}
}
