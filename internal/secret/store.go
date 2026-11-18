// Package secret abstracts where provider API keys are stored. The production
// backend is the OS keychain (macOS Keychain / libsecret / Windows Credential
// Manager) via zalando/go-keyring. Plaintext keys live only in the keychain
// and in process memory — never on disk.
package secret

import (
	"errors"
	"fmt"

	"github.com/zalando/go-keyring"
)

// ErrNotFound is returned when a secret entry does not exist.
var ErrNotFound = errors.New("secret: not found")

// Service is the keychain service name namespace for agentvault entries.
const Service = "agentvault"

// Store is the secret backend interface.
type Store interface {
	// Set stores secret under service/account.
	Set(service, account, secret string) error
	// Get retrieves the secret for service/account.
	Get(service, account string) (string, error)
	// Delete removes the secret entry.
	Delete(service, account string) error
}

// KeyringStore is the production backend backed by the OS keychain.
type KeyringStore struct{}

// Set stores secret in the OS keychain.
func (KeyringStore) Set(service, account, secret string) error {
	if err := keyring.Set(service, account, secret); err != nil {
		return fmt.Errorf("secret: keyring set: %w", err)
	}
	return nil
}

// Get reads the secret from the OS keychain.
func (KeyringStore) Get(service, account string) (string, error) {
	v, err := keyring.Get(service, account)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("secret: keyring get: %w", err)
	}
	return v, nil
}

// Delete removes the entry from the OS keychain.
func (KeyringStore) Delete(service, account string) error {
	err := keyring.Delete(service, account)
	if errors.Is(err, keyring.ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("secret: keyring delete: %w", err)
	}
	return nil
}

// MemoryStore is an in-memory backend for tests and CI environments without
// a desktop keychain. It must never be used as the production default: data
// vanishes with the process and is plainly readable in memory dumps.
type MemoryStore struct {
	entries map[string]string
}

// defaultMemory is the process-wide store returned by SharedMemoryStore so
// that every component selecting the memory backend within one process sees
// the same entries (mirroring how a real keychain is process-global).
var defaultMemory = NewMemoryStore()

// NewMemoryStore returns an empty in-memory store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{entries: map[string]string{}}
}

// SharedMemoryStore returns the process-wide memory store.
func SharedMemoryStore() *MemoryStore { return defaultMemory }

func memKey(service, account string) string { return service + "/" + account }

// Set stores the secret in memory.
func (m *MemoryStore) Set(service, account, secret string) error {
	m.entries[memKey(service, account)] = secret
	return nil
}

// Get reads the secret from memory.
func (m *MemoryStore) Get(service, account string) (string, error) {
	v, ok := m.entries[memKey(service, account)]
	if !ok {
		return "", ErrNotFound
	}
	return v, nil
}

// Delete removes the entry from memory.
func (m *MemoryStore) Delete(service, account string) error {
	if _, ok := m.entries[memKey(service, account)]; !ok {
		return ErrNotFound
	}
	delete(m.entries, memKey(service, account))
	return nil
}
