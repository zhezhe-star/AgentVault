// Package crypto implements envelope encryption over the secret store: a
// data-encryption key (DEK) is generated once, stored in the OS keychain via
// internal/secret, and used to AES-GCM-encrypt any sensitive values that must
// be persisted outside the keychain (e.g. base_url fragments carrying
// credentials). Provider API keys themselves never pass through here — they
// live only in the keychain.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"

	"github.com/zhezhe-star/AgentVault/internal/secret"
)

// ErrBadCiphertext is returned when ciphertext fails authentication.
var ErrBadCiphertext = errors.New("crypto: ciphertext authentication failed")

// dekAccount is the keychain account holding the DEK for a given store
// service namespace.
const dekAccount = "data-encryption-key"

// DEKSize is the AES-256 key length in bytes.
const DEKSize = 32

// Envelope encrypts/decrypts values with a DEK resolved through a
// secret.Store, generating it on first use.
type Envelope struct {
	store secret.Store
	dek   []byte
}

// New resolves (or creates) the DEK in the keychain and returns an Envelope.
func New(store secret.Store, service string) (*Envelope, error) {
	e := &Envelope{store: store}
	dek, err := store.Get(service, dekAccount)
	switch {
	case errors.Is(err, secret.ErrNotFound):
		raw := make([]byte, DEKSize)
		if _, err := io.ReadFull(rand.Reader, raw); err != nil {
			return nil, fmt.Errorf("crypto: generate DEK: %w", err)
		}
		if err := store.Set(service, dekAccount, encode(raw)); err != nil {
			return nil, fmt.Errorf("crypto: store DEK: %w", err)
		}
		e.dek = raw
	case err != nil:
		return nil, fmt.Errorf("crypto: load DEK: %w", err)
	default:
		raw, err := decode(dek)
		if err != nil {
			return nil, fmt.Errorf("crypto: decode stored DEK: %w", err)
		}
		if len(raw) != DEKSize {
			return nil, fmt.Errorf("crypto: stored DEK has %d bytes, want %d", len(raw), DEKSize)
		}
		e.dek = raw
	}
	return e, nil
}

// Encrypt seals plaintext into a base64(nonce|ciphertext) string. Each call
// uses a fresh random nonce, so identical plaintexts produce different
// ciphertexts.
func (e *Envelope) Encrypt(plaintext []byte) (string, error) {
	block, err := aes.NewCipher(e.dek)
	if err != nil {
		return "", fmt.Errorf("crypto: new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("crypto: new GCM: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("crypto: nonce: %w", err)
	}
	sealed := gcm.Seal(nonce, nonce, plaintext, nil)
	return encode(sealed), nil
}

// Decrypt opens a value produced by Encrypt.
func (e *Envelope) Decrypt(ciphertext string) ([]byte, error) {
	sealed, err := decode(ciphertext)
	if err != nil {
		return nil, fmt.Errorf("crypto: input: %w", err)
	}
	block, err := aes.NewCipher(e.dek)
	if err != nil {
		return nil, fmt.Errorf("crypto: new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto: new GCM: %w", err)
	}
	if len(sealed) < gcm.NonceSize() {
		return nil, ErrBadCiphertext
	}
	nonce, body := sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, body, nil)
	if err != nil {
		return nil, ErrBadCiphertext
	}
	return plaintext, nil
}

func encode(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func decode(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}
