package migrate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixtureHome builds a temp HOME with the four known plaintext drop points.
func fixtureHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(".zshrc", `# shell config
export PATH=/usr/local/bin:$PATH
export ARK_API_KEY="ak-volc-abcdef1234567890abcdef"
export GLM_REF="${ARK_API_KEY}"
export EDITOR=vim
alias ll='ls -la'
`)
	settings := map[string]any{
		"env": map[string]string{
			"ANTHROPIC_BASE_URL":     "http://127.0.0.1:8787",
			"ANTHROPIC_AUTH_TOKEN":   "agentvault-local-placeholder",
			"ANTHROPIC_LEAKED_TOKEN": "sk-ant-leakedkey1234567890",
			"MY_TOOL_TOKEN":          "tok-zai-abcdef1234567890abcdef",
		},
	}
	b, _ := json.Marshal(settings)
	write(".claude/settings.json", string(b))
	write(".config/litellm/config.yaml", `model_list:
  - model_name: ark-model
    litellm_params:
      model: openai/ark-model
      api_key: ak-volc-fedcba0987654321fedcba
  - model_name: glm
    litellm_params:
      model: openai/glm
      api_key: ${BIGMODEL_KEY}
`)
	write(".codex/config.toml", `[mcp_servers.ark]
command = "arkcli"

[mcp_servers.ark.env]
ARK_API_KEY = "ak-volc-1111222233334444aaaa"

[mcp_servers.zai.env]
Z_AI_API_KEY = "zai-key-abcdef12345678901234"
`)
	return home
}

func TestScanFindsPlaintextAndMasks(t *testing.T) {
	s := NewScanner(fixtureHome(t))
	found, err := s.Scan()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range found {
		got = append(got, f.Var)
	}
	want := []string{"ARK_API_KEY", "ANTHROPIC_LEAKED_TOKEN", "MY_TOOL_TOKEN", "api_key", "ARK_API_KEY", "Z_AI_API_KEY"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("scan vars = %v, want %v", got, want)
	}
	for _, f := range found {
		if strings.Contains(f.MaskedValue, "abcdef1234567890abcdef") && !strings.Contains(f.MaskedValue, "…") {
			t.Fatalf("%s: masked value leaks full secret: %s", f.Var, f.MaskedValue)
		}
		if len(f.MaskedValue) > 24 {
			t.Fatalf("%s: masked value too long: %s", f.Var, f.MaskedValue)
		}
	}
}

func TestScanIgnoresPlaceholdersAndNonCredentials(t *testing.T) {
	s := NewScanner(fixtureHome(t))
	found, err := s.Scan()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range found {
		if f.Var == "ANTHROPIC_AUTH_TOKEN" {
			t.Fatal("placeholder token must not be reported")
		}
		if f.Var == "PATH" || f.Var == "EDITOR" || f.Var == "GLM_REF" {
			t.Fatalf("non-credential var %s must not be reported", f.Var)
		}
	}
}

func TestScanClassesDefaultAndOverride(t *testing.T) {
	home := fixtureHome(t)
	s := NewScanner(home)
	found, err := s.Scan()
	if err != nil {
		t.Fatal(err)
	}
	byVar := map[string]Finding{}
	for _, f := range found {
		byVar[f.Var] = f
	}
	if byVar["api_key"].Class != ClassProxyAttach { // litellm config
		t.Fatalf("litellm api_key class = %s", byVar["api_key"].Class)
	}
	if byVar["ARK_API_KEY"].Class != ClassRunInject { // shell / toml env
		t.Fatalf("shell ARK_API_KEY class = %s", byVar["ARK_API_KEY"].Class)
	}

	// Override wins.
	s2 := NewScanner(home)
	s2.SetClassification("ARK_API_KEY", ClassCompliant)
	found2, _ := s2.Scan()
	for _, f := range found2 {
		if f.Var == "ARK_API_KEY" && f.Class != ClassCompliant {
			t.Fatalf("override not applied: %s", f.Class)
		}
	}
}

func TestScanMissingHomeIsEmpty(t *testing.T) {
	found, err := NewScanner(t.TempDir()).Scan()
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Fatalf("empty home should find nothing, got %v", found)
	}
}

func TestMask(t *testing.T) {
	long := "abcdefghijklmnop"
	m := Mask(long)
	if !strings.HasPrefix(m, "abcd") || !strings.Contains(m, "mnop") || !strings.Contains(m, "…") {
		t.Fatalf("Mask(long) = %s", m)
	}
	if !strings.Contains(Mask("short"), "chars") {
		t.Fatalf("Mask(short) = %s", Mask("short"))
	}
}
