// Package provider implements the provider/key entity model and its
// SQLite-backed store with forward-only schema migrations.
//
// Secrets never enter this database: the keys table stores only a secret_id
// that references an entry in the SecretStore (OS keychain). See
// internal/secret.
package provider

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, registers "sqlite"
)

// ErrNotFound is returned when a provider or key does not exist.
var ErrNotFound = errors.New("provider: not found")

// Key status values.
const (
	KeyActive   = "active"
	KeyDisabled = "disabled"
	KeyRevoked  = "revoked"
)

// Provider is a model provider (e.g. bigmodel, deepseek, volcengine ark).
type Provider struct {
	ID      string
	Name    string
	BaseURL string
	// AnthropicBaseURL is the provider's Anthropic-compatible endpoint
	// (e.g. https://open.bigmodel.cn/api/anthropic), used by the
	// /v1/messages passthrough. Empty means the provider is OpenAI-only.
	AnthropicBaseURL string
	CreatedAt        time.Time
}

// Key is one credential slot of a provider. The actual secret lives in the
// OS keychain; only its reference is persisted here.
type Key struct {
	ID         string
	ProviderID string
	SecretID   string // reference into SecretStore, never a plaintext key
	Status     string // KeyActive | KeyDisabled | KeyRevoked
	Weight     int
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Model is a model mapping registered under a provider key.
type Model struct {
	ID         string
	ProviderID string
	KeyID      string
	Name       string // model name as clients request it, e.g. "glm-5.3"
	Upstream   string // model id at the provider, e.g. "glm-5.3-flash"
}

// migrations are applied in order; each runs exactly once, tracked in
// schema_migrations. Forward-only by design.
var migrations = []string{
	`CREATE TABLE providers (
		id         TEXT PRIMARY KEY,
		name       TEXT NOT NULL,
		base_url   TEXT NOT NULL,
		created_at TEXT NOT NULL
	)`,
	`CREATE TABLE keys (
		id          TEXT PRIMARY KEY,
		provider_id TEXT NOT NULL REFERENCES providers(id),
		secret_id   TEXT NOT NULL,
		status      TEXT NOT NULL DEFAULT 'active',
		weight      INTEGER NOT NULL DEFAULT 1,
		created_at  TEXT NOT NULL,
		updated_at  TEXT NOT NULL
	)`,
	`CREATE TABLE models (
		id          TEXT PRIMARY KEY,
		provider_id TEXT NOT NULL REFERENCES providers(id),
		key_id      TEXT NOT NULL REFERENCES keys(id),
		name        TEXT NOT NULL,
		upstream    TEXT NOT NULL,
		UNIQUE(key_id, name)
	)`,
	`CREATE INDEX idx_keys_provider ON keys(provider_id)`,
	`CREATE INDEX idx_models_key ON models(key_id)`,
	// Migration 4: optional Anthropic-compatible endpoint per provider for
	// the /v1/messages passthrough. Empty string = OpenAI-only provider.
	`ALTER TABLE providers ADD COLUMN anthropic_base_url TEXT NOT NULL DEFAULT ''`,
	// Migration 5: token quotas. scope is "provider" or "key", ref_id the
	// provider id / key id; max_tokens counted over a rolling window.
	`CREATE TABLE quotas (
		scope          TEXT NOT NULL,
		ref_id         TEXT NOT NULL,
		window_seconds INTEGER NOT NULL,
		max_tokens     INTEGER NOT NULL,
		PRIMARY KEY (scope, ref_id)
	)`,
}

// Store wraps the SQLite database.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the database at path and applies pending
// migrations. Safe to call repeatedly.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("provider: open db: %w", err)
	}
	// WAL keeps usage writes from blocking reads; a busy timeout smooths
	// concurrent access from the proxy and CLI.
	for _, pragma := range []string{"PRAGMA journal_mode=WAL", "PRAGMA busy_timeout=5000"} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("provider: pragma: %w", err)
		}
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("provider: create schema_migrations: %w", err)
	}
	var current int
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("provider: read schema version: %w", err)
	}
	for i, m := range migrations {
		version := i + 1
		if version <= current {
			continue
		}
		tx, err := s.db.Begin()
		if err != nil {
			return fmt.Errorf("provider: begin migration %d: %w", version, err)
		}
		if _, err := tx.Exec(m); err != nil {
			tx.Rollback()
			return fmt.Errorf("provider: apply migration %d: %w", version, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
			version, time.Now().UTC().Format(time.RFC3339)); err != nil {
			tx.Rollback()
			return fmt.Errorf("provider: record migration %d: %w", version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("provider: commit migration %d: %w", version, err)
		}
	}
	return nil
}

// CreateProvider inserts a new provider.
func (s *Store) CreateProvider(p Provider) error {
	p.CreatedAt = time.Now().UTC()
	_, err := s.db.Exec(`INSERT INTO providers (id, name, base_url, anthropic_base_url, created_at) VALUES (?,?,?,?,?)`,
		p.ID, p.Name, p.BaseURL, p.AnthropicBaseURL, p.CreatedAt.Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("provider: create provider: %w", err)
	}
	return nil
}

// ListProviders returns all providers ordered by creation time.
func (s *Store) ListProviders() ([]Provider, error) {
	rows, err := s.db.Query(`SELECT id, name, base_url, anthropic_base_url, created_at FROM providers ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("provider: list providers: %w", err)
	}
	defer rows.Close()
	var out []Provider
	for rows.Next() {
		var p Provider
		var created string
		if err := rows.Scan(&p.ID, &p.Name, &p.BaseURL, &p.AnthropicBaseURL, &created); err != nil {
			return nil, fmt.Errorf("provider: scan provider: %w", err)
		}
		p.CreatedAt, _ = time.Parse(time.RFC3339, created)
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetProvider fetches one provider by ID.
func (s *Store) GetProvider(id string) (Provider, error) {
	var p Provider
	var created string
	err := s.db.QueryRow(`SELECT id, name, base_url, anthropic_base_url, created_at FROM providers WHERE id = ?`, id).
		Scan(&p.ID, &p.Name, &p.BaseURL, &p.AnthropicBaseURL, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	if err != nil {
		return p, fmt.Errorf("provider: get provider: %w", err)
	}
	p.CreatedAt, _ = time.Parse(time.RFC3339, created)
	return p, nil
}

// CreateKey inserts a new key slot for a provider.
func (s *Store) CreateKey(k Key) error {
	now := time.Now().UTC()
	k.CreatedAt, k.UpdatedAt = now, now
	_, err := s.db.Exec(`INSERT INTO keys (id, provider_id, secret_id, status, weight, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?)`,
		k.ID, k.ProviderID, k.SecretID, k.Status, k.Weight,
		k.CreatedAt.Format(time.RFC3339), k.UpdatedAt.Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("provider: create key: %w", err)
	}
	return nil
}

// ListKeys returns the keys of a provider.
func (s *Store) ListKeys(providerID string) ([]Key, error) {
	rows, err := s.db.Query(`SELECT id, provider_id, secret_id, status, weight, created_at, updated_at
		FROM keys WHERE provider_id = ? ORDER BY created_at`, providerID)
	if err != nil {
		return nil, fmt.Errorf("provider: list keys: %w", err)
	}
	defer rows.Close()
	return scanKeys(rows)
}

// ListActiveKeys returns keys in active status across all providers, for routing.
func (s *Store) ListActiveKeys() ([]Key, error) {
	rows, err := s.db.Query(`SELECT id, provider_id, secret_id, status, weight, created_at, updated_at
		FROM keys WHERE status = 'active' ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("provider: list active keys: %w", err)
	}
	defer rows.Close()
	return scanKeys(rows)
}

func scanKeys(rows *sql.Rows) ([]Key, error) {
	var out []Key
	for rows.Next() {
		var k Key
		var created, updated string
		if err := rows.Scan(&k.ID, &k.ProviderID, &k.SecretID, &k.Status, &k.Weight, &created, &updated); err != nil {
			return nil, fmt.Errorf("provider: scan key: %w", err)
		}
		k.CreatedAt, _ = time.Parse(time.RFC3339, created)
		k.UpdatedAt, _ = time.Parse(time.RFC3339, updated)
		out = append(out, k)
	}
	return out, rows.Err()
}

// SetKeyStatus transitions a key's status (active/disabled/revoked).
func (s *Store) SetKeyStatus(keyID, status string) error {
	res, err := s.db.Exec(`UPDATE keys SET status = ?, updated_at = ? WHERE id = ?`,
		status, time.Now().UTC().Format(time.RFC3339), keyID)
	if err != nil {
		return fmt.Errorf("provider: set key status: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// CreateModel registers a model mapping under a key.
func (s *Store) CreateModel(m Model) error {
	_, err := s.db.Exec(`INSERT INTO models (id, provider_id, key_id, name, upstream) VALUES (?,?,?,?,?)`,
		m.ID, m.ProviderID, m.KeyID, m.Name, m.Upstream)
	if err != nil {
		return fmt.Errorf("provider: create model: %w", err)
	}
	return nil
}

// ListModels returns the model mappings of a provider.
func (s *Store) ListModels(providerID string) ([]Model, error) {
	rows, err := s.db.Query(`SELECT id, provider_id, key_id, name, upstream FROM models
		WHERE provider_id = ? ORDER BY name`, providerID)
	if err != nil {
		return nil, fmt.Errorf("provider: list models: %w", err)
	}
	defer rows.Close()
	var out []Model
	for rows.Next() {
		var m Model
		if err := rows.Scan(&m.ID, &m.ProviderID, &m.KeyID, &m.Name, &m.Upstream); err != nil {
			return nil, fmt.Errorf("provider: scan model: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Quota is a rolling-window token budget for a provider or a key.
type Quota struct {
	Scope         string // "provider" | "key"
	RefID         string
	WindowSeconds int
	MaxTokens     int64
}

// SetQuota upserts a quota entry.
func (s *Store) SetQuota(q Quota) error {
	_, err := s.db.Exec(`INSERT INTO quotas (scope, ref_id, window_seconds, max_tokens) VALUES (?,?,?,?)
		ON CONFLICT(scope, ref_id) DO UPDATE SET window_seconds=excluded.window_seconds, max_tokens=excluded.max_tokens`,
		q.Scope, q.RefID, q.WindowSeconds, q.MaxTokens)
	if err != nil {
		return fmt.Errorf("provider: set quota: %w", err)
	}
	return nil
}

// DeleteQuota removes a quota entry.
func (s *Store) DeleteQuota(scope, refID string) error {
	_, err := s.db.Exec(`DELETE FROM quotas WHERE scope = ? AND ref_id = ?`, scope, refID)
	if err != nil {
		return fmt.Errorf("provider: delete quota: %w", err)
	}
	return nil
}

// ListQuotas returns every configured quota.
func (s *Store) ListQuotas() ([]Quota, error) {
	rows, err := s.db.Query(`SELECT scope, ref_id, window_seconds, max_tokens FROM quotas`)
	if err != nil {
		return nil, fmt.Errorf("provider: list quotas: %w", err)
	}
	defer rows.Close()
	var out []Quota
	for rows.Next() {
		var q Quota
		if err := rows.Scan(&q.Scope, &q.RefID, &q.WindowSeconds, &q.MaxTokens); err != nil {
			return nil, fmt.Errorf("provider: scan quota: %w", err)
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// ListAllModels returns every model mapping across providers.
func (s *Store) ListAllModels() ([]Model, error) {
	rows, err := s.db.Query(`SELECT id, provider_id, key_id, name, upstream FROM models ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("provider: list all models: %w", err)
	}
	defer rows.Close()
	var out []Model
	for rows.Next() {
		var m Model
		if err := rows.Scan(&m.ID, &m.ProviderID, &m.KeyID, &m.Name, &m.Upstream); err != nil {
			return nil, fmt.Errorf("provider: scan model: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
