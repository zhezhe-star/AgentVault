package crypto

import (
	"bytes"
	"errors"
	"testing"

	"github.com/zhezhe-star/AgentVault/internal/secret"
)

const testService = "agentvault-test"

func newTestEnvelope(t *testing.T, store secret.Store) *Envelope {
	t.Helper()
	e, err := New(store, testService)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

func TestRoundTrip(t *testing.T) {
	store := secret.NewMemoryStore()
	e := newTestEnvelope(t, store)

	ct, err := e.Encrypt([]byte("sensitive-base-url-fragment"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	pt, err := e.Decrypt(ct)
	if err != nil || string(pt) != "sensitive-base-url-fragment" {
		t.Fatalf("Decrypt: %v %q", err, pt)
	}
}

func TestTamperedCiphertextFails(t *testing.T) {
	e := newTestEnvelope(t, secret.NewMemoryStore())

	ct, err := e.Encrypt([]byte("payload"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	raw, _ := decode(ct)
	raw[len(raw)-1] ^= 0xff
	if _, err := e.Decrypt(encode(raw)); !errors.Is(err, ErrBadCiphertext) {
		t.Fatalf("Decrypt(tampered) err = %v, want ErrBadCiphertext", err)
	}
}

func TestWrongDEKFails(t *testing.T) {
	ct, err := newTestEnvelope(t, secret.NewMemoryStore()).Encrypt([]byte("payload"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	// A fresh store resolves a different DEK.
	other := newTestEnvelope(t, secret.NewMemoryStore())
	if _, err := other.Decrypt(ct); !errors.Is(err, ErrBadCiphertext) {
		t.Fatalf("Decrypt(wrong DEK) err = %v, want ErrBadCiphertext", err)
	}
}

func TestCiphertextRandomized(t *testing.T) {
	e := newTestEnvelope(t, secret.NewMemoryStore())
	ct1, err := e.Encrypt([]byte("same plaintext"))
	if err != nil {
		t.Fatalf("Encrypt 1: %v", err)
	}
	ct2, err := e.Encrypt([]byte("same plaintext"))
	if err != nil {
		t.Fatalf("Encrypt 2: %v", err)
	}
	if ct1 == ct2 {
		t.Fatal("identical ciphertexts for identical plaintexts; nonce reuse suspected")
	}
	pt1, _ := e.Decrypt(ct1)
	pt2, _ := e.Decrypt(ct2)
	if !bytes.Equal(pt1, pt2) {
		t.Fatal("decrypted plaintexts differ")
	}
}

func TestDEKReuseAcrossEnvelopes(t *testing.T) {
	// Two envelopes sharing a store must resolve the same keychain DEK,
	// not generate a second one.
	store := secret.NewMemoryStore()
	e1 := newTestEnvelope(t, store)
	ct, err := e1.Encrypt([]byte("shared"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	e2 := newTestEnvelope(t, store)
	pt, err := e2.Decrypt(ct)
	if err != nil || string(pt) != "shared" {
		t.Fatalf("cross-envelope decrypt: %v %q", err, pt)
	}
}
