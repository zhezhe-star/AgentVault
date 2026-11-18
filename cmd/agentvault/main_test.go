package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhezhe-star/AgentVault/internal/app"
)

// runCLI executes a command against a sandbox AGENTVAULT_HOME with the
// in-memory secret backend, returning combined stdout.
func runCLI(t *testing.T, sandboxHome string, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	root := newRootCmd(&out)
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	t.Setenv("AGENTVAULT_HOME", sandboxHome)
	t.Setenv("AGENTVAULT_SECRET_BACKEND", "memory")
	if err := root.Execute(); err != nil {
		t.Fatalf("agentvault %v: %v\noutput: %s", args, err, out.String())
	}
	return out.String()
}

func sandbox(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() { os.Unsetenv("AGENTVAULT_HOME") })
	return dir
}

// TestCLIProviderLifecycle covers add -> add-key -> list/show -> revoke,
// asserting no plaintext key ever reaches stdout/stderr.
func TestCLIProviderLifecycle(t *testing.T) {
	home := sandbox(t)
	const fakeKey = "av-test-0123456789abcdef-not-a-real-key"

	runCLI(t, home, "provider", "add", "testprov", "--name", "Test Provider", "--base-url", "https://upstream.example.com")

	// add-key reads the secret from stdin (no echo path): feed it directly.
	var stdin bytes.Buffer
	stdin.WriteString(fakeKey + "\n")
	var out bytes.Buffer
	root := newRootCmd(&out)
	root.SetIn(&stdin)
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"add-key", "testprov", "--key-id", "k1"})
	t.Setenv("AGENTVAULT_HOME", home)
	t.Setenv("AGENTVAULT_SECRET_BACKEND", "memory")
	if err := root.Execute(); err != nil {
		t.Fatalf("add-key: %v", err)
	}

	// The secret must be retrievable from the configured backend.
	cfg, err := app.Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	got, err := cfg.Secret.Get(cfg.Service, "testprov/k1")
	if err != nil || got != fakeKey {
		t.Fatalf("secret store roundtrip: %v %q", err, got)
	}

	// list and show must expose metadata and references, never the secret.
	for _, args := range [][]string{{"provider", "list"}, {"provider", "show", "testprov"}} {
		shown := runCLI(t, home, args...)
		if !strings.Contains(shown, "testprov") {
			t.Fatalf("%v output missing provider id: %s", args, shown)
		}
		if strings.Contains(shown, fakeKey) {
			t.Fatalf("%v output leaked plaintext key: %s", args, shown)
		}
	}
	if shown := runCLI(t, home, "provider", "show", "testprov"); !strings.Contains(shown, "ref:testprov/k1") {
		t.Fatalf("show output missing key ref: %s", shown)
	}

	// revoke flips the status; show reflects it without leaking the secret.
	runCLI(t, home, "provider", "revoke", "testprov", "k1")
	shown := runCLI(t, home, "provider", "show", "testprov")
	if !strings.Contains(shown, "status=revoked") {
		t.Fatalf("show after revoke: %s", shown)
	}
	if strings.Contains(shown, fakeKey) {
		t.Fatal("show after revoke leaked plaintext key")
	}
}

// TestSandboxIsolation ensures the sandbox never touches the real user config.
func TestSandboxIsolation(t *testing.T) {
	home := sandbox(t)
	runCLI(t, home, "provider", "add", "iso", "--name", "Iso", "--base-url", "https://x.example.com")
	if _, err := os.Stat(filepath.Join(home, "agentvault.db")); err != nil {
		t.Fatalf("sandbox db not created: %v", err)
	}
}
