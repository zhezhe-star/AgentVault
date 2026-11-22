package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhezhe-star/AgentVault/internal/app"
	"github.com/zhezhe-star/AgentVault/internal/secret"
)

// runTestCfg builds a config whose AGENTVAULT_HOME points at a temp dir with
// a memory secret store holding one seeded ref.
func runTestCfg(t *testing.T) (app.Config, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("AGENTVAULT_HOME", home)
	cfg, err := app.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	store := secret.NewMemoryStore()
	if err := store.Set(cfg.Service, "ark/ark_api_key", "ak-volc-abcdef1234567890abcdef"); err != nil {
		t.Fatal(err)
	}
	cfg.Secret = store
	return cfg, home
}

func TestRunInjectsChildEnvOnly(t *testing.T) {
	cfg, home := runTestCfg(t)
	envs, err := buildInjectedEnv(cfg, []string{"ARK_API_KEY=ref:ark/ark_api_key"}, []string{"PATH=/usr/bin", "HOME=" + home})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(envs, "\n")
	if !strings.Contains(joined, "ARK_API_KEY=ak-volc-abcdef1234567890abcdef") {
		t.Fatalf("child env missing injected var: %s", joined)
	}
	// Parent environment must stay clean.
	for _, e := range os.Environ() {
		if strings.Contains(e, "ak-volc-abcdef1234567890abcdef") {
			t.Fatalf("plaintext leaked into parent environment: %s", e)
		}
	}
}

func TestRunRejectsBadSpecAndMissingRef(t *testing.T) {
	cfg, _ := runTestCfg(t)
	if _, err := buildInjectedEnv(cfg, []string{"NO_REF=plain-value"}, nil); err == nil {
		t.Fatal("spec without ref: must be rejected")
	}
	if _, err := buildInjectedEnv(cfg, []string{"V=ref:missing/ref"}, nil); err == nil {
		t.Fatal("unknown ref must be rejected")
	}
}

func TestRunExecutesChildWithInjectedValue(t *testing.T) {
	cfg, home := runTestCfg(t)
	envs, err := buildInjectedEnv(cfg, []string{"ARK_API_KEY=ref:ark/ark_api_key"}, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	// printenv semantics: the child must see exactly the injected value.
	script := filepath.Join(home, "printenv.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintenv ARK_API_KEY\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	out := runChild(t, script, envs)
	if strings.TrimSpace(out) != "ak-volc-abcdef1234567890abcdef" {
		t.Fatalf("child saw %q", out)
	}
}
