// Package leakscan holds the anti-leak test suite: inject a synthetic key
// into the full pipeline (store -> secret backend -> outputs) and assert it
// never appears in the source tree, SQLite bytes, command output or error
// strings. This is the persistent guard behind plan.md's invariant that
// plaintext keys live only in the keychain and process memory.
package leakscan

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhezhe-star/AgentVault/internal/app"
	"github.com/zhezhe-star/AgentVault/internal/provider"

	cryptoenv "github.com/zhezhe-star/AgentVault/internal/crypto"
)

// syntheticKey returns a random well-formed key so no real credential is
// ever used in tests.
func syntheticKey(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return "av-test-" + hex.EncodeToString(raw)
}

// scanTree walks a directory (skipping .git and bin) and reports any file
// containing needle.
func scanTree(t *testing.T, root, needle string) []string {
	t.Helper()
	var hits []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".git" || name == "bin" {
				return filepath.SkipDir
			}
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(data), needle) {
			hits = append(hits, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan tree %s: %v", root, err)
	}
	return hits
}

func TestNoKeyLeaks(t *testing.T) {
	key := syntheticKey(t)
	sandbox := t.TempDir()
	t.Setenv("AGENTVAULT_HOME", sandbox)
	t.Setenv("AGENTVAULT_SECRET_BACKEND", "memory")

	cfg, err := app.Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	// 1. Persist metadata referencing the key...
	store, err := cfg.OpenStore()
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	if err := store.CreateProvider(provider.Provider{ID: "leakprov", Name: "LeakScan", BaseURL: "https://upstream.example.com"}); err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}
	if err := store.CreateKey(provider.Key{ID: "k1", ProviderID: "leakprov", SecretID: app.KeyRef("leakprov", "k1"), Status: provider.KeyActive, Weight: 1}); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	store.Close()

	// 2. ...store the secret in the backend...
	if err := cfg.Secret.Set(cfg.Service, "leakprov/k1", key); err != nil {
		t.Fatalf("Secret.Set: %v", err)
	}

	// 3. ...and exercise the envelope layer with the same value.
	env, err := cryptoenv.New(cfg.Secret, cfg.Service)
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	ct, err := env.Encrypt([]byte(key))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	got, err := env.Decrypt(ct)
	if err != nil || string(got) != key {
		t.Fatalf("envelope roundtrip: %v", err)
	}

	// Assertion 1: SQLite bytes must not contain the plaintext key.
	dbBytes, err := os.ReadFile(cfg.DBPath)
	if err != nil {
		t.Fatalf("read db: %v", err)
	}
	if strings.Contains(string(dbBytes), key) {
		t.Fatal("LEAK: plaintext key found in SQLite bytes")
	}
	// WAL sidecar files count as the database too.
	if wal, err := os.ReadFile(cfg.DBPath + "-wal"); err == nil {
		if strings.Contains(string(wal), key) {
			t.Fatal("LEAK: plaintext key found in SQLite WAL")
		}
	}

	// Assertion 2: the source tree must not contain the key (it is random,
	// so a hit would mean something wrote it to disk).
	for _, hit := range scanTree(t, "..", key) {
		t.Fatalf("LEAK: plaintext key found in %s", hit)
	}

	// Assertion 3: error messages and structured output must not embed it.
	_, err = env.Decrypt("garbage")
	if err != nil && strings.Contains(err.Error(), key) {
		t.Fatal("LEAK: plaintext key embedded in error message")
	}
	var out strings.Builder
	p, err := store.GetProvider("leakprov") // store closed; expect an error path
	_ = p
	if err != nil {
		out.WriteString(err.Error())
	}
	if strings.Contains(out.String(), key) {
		t.Fatal("LEAK: plaintext key embedded in output")
	}
}
