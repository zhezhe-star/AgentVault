// Package app wires the provider store, secret store and CLI-facing
// operations together with runtime configuration resolution.
package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/zhezhe-star/AgentVault/internal/crypto"
	"github.com/zhezhe-star/AgentVault/internal/provider"
	"github.com/zhezhe-star/AgentVault/internal/secret"
)

// Config holds resolved runtime paths and backend selection.
type Config struct {
	DBPath  string
	Secret  secret.Store
	Service string // keychain service namespace
	DataDir string
}

// ErrFlagMissing is returned when a required flag/value is absent.
var ErrFlagMissing = errors.New("app: required value missing")

// DefaultDataDir returns the per-user data directory
// (~/.config/agentvault, or $AGENTVAULT_HOME for tests/sandboxes).
func DefaultDataDir() (string, error) {
	if v := os.Getenv("AGENTVAULT_HOME"); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("app: resolve home: %w", err)
	}
	return filepath.Join(home, ".config", "agentvault"), nil
}

// Resolve builds the runtime config. Secret backend selection: env var
// AGENTVAULT_SECRET_BACKEND=memory forces the in-memory store (tests/CI only);
// otherwise the OS keychain is used.
func Resolve() (Config, error) {
	dir, err := DefaultDataDir()
	if err != nil {
		return Config{}, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Config{}, fmt.Errorf("app: create data dir: %w", err)
	}
	cfg := Config{
		DataDir: dir,
		DBPath:  filepath.Join(dir, "agentvault.db"),
		Service: "agentvault",
		Secret:  secret.KeyringStore{},
	}
	if os.Getenv("AGENTVAULT_SECRET_BACKEND") == "memory" {
		cfg.Secret = secret.SharedMemoryStore()
	}
	return cfg, nil
}

// OpenStore opens the provider store at cfg.DBPath.
func (c Config) OpenStore() (*provider.Store, error) {
	return provider.Open(c.DBPath)
}

// NewEnvelope builds the envelope encryptor for cfg.
func (c Config) NewEnvelope() (*crypto.Envelope, error) {
	return crypto.New(c.Secret, c.Service)
}

// KeyRef is the canonical reference to a stored key:
// ref:<provider>/<key-id>. Config files and logs may contain this reference;
// they must never contain the underlying secret.
func KeyRef(providerID, keyID string) string {
	return fmt.Sprintf("ref:%s/%s", providerID, keyID)
}
