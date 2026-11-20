// Package usage persists per-call usage records: one row per proxied
// request with provider/key/model, token counts, latency and status. It
// opens the same SQLite file as the provider store (WAL allows concurrent
// connections) but keeps its own forward-only migration marker.
package usage

import (
	"database/sql"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, registers "sqlite"
)

// Record is one proxied call.
type Record struct {
	ProviderID string
	KeyID      string
	Model      string
	Protocol   string // "openai" | "anthropic"
	Status     int    // final status relayed to the client
	TokensIn   int64
	TokensOut  int64
	LatencyMS  int64
	CreatedAt  time.Time
}

// Store wraps the SQLite database for usage writes/reads.
type Store struct {
	db *sql.DB
	// writeMu serializes writes: busy_timeout applies per pooled
	// connection, so concurrent writers on fresh connections would hit
	// SQLITE_BUSY before the timeout machinery engages.
	writeMu sync.Mutex
}

// Open opens (creating if needed) the usage schema in the database file at
// path. Safe to call repeatedly and from a second connection while the
// provider store holds the same file open.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("usage: open db: %w", err)
	}
	for _, pragma := range []string{"PRAGMA journal_mode=WAL", "PRAGMA busy_timeout=5000"} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("usage: pragma: %w", err)
		}
	}
	const create = `CREATE TABLE IF NOT EXISTS usage_records (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		created_at  TEXT NOT NULL,
		provider_id TEXT NOT NULL,
		key_id      TEXT NOT NULL,
		model       TEXT NOT NULL,
		protocol    TEXT NOT NULL,
		status      INTEGER NOT NULL,
		tokens_in   INTEGER NOT NULL DEFAULT 0,
		tokens_out  INTEGER NOT NULL DEFAULT 0,
		latency_ms  INTEGER NOT NULL DEFAULT 0
	)`
	if _, err := db.Exec(create); err != nil {
		db.Close()
		return nil, fmt.Errorf("usage: create table: %w", err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_usage_created ON usage_records(created_at)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("usage: create index: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Insert persists one record.
func (s *Store) Insert(r Record) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.Exec(`INSERT INTO usage_records
		(created_at, provider_id, key_id, model, protocol, status, tokens_in, tokens_out, latency_ms)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		r.CreatedAt.Format(time.RFC3339Nano), r.ProviderID, r.KeyID, r.Model,
		r.Protocol, r.Status, r.TokensIn, r.TokensOut, r.LatencyMS)
	if err != nil {
		return fmt.Errorf("usage: insert: %w", err)
	}
	return nil
}

// Query returns records ordered newest-first, capped at limit (0 = 100).
func (s *Store) Query(limit int) ([]Record, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT created_at, provider_id, key_id, model, protocol, status, tokens_in, tokens_out, latency_ms
		FROM usage_records ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("usage: query: %w", err)
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		var r Record
		var created string
		if err := rows.Scan(&created, &r.ProviderID, &r.KeyID, &r.Model, &r.Protocol,
			&r.Status, &r.TokensIn, &r.TokensOut, &r.LatencyMS); err != nil {
			return nil, fmt.Errorf("usage: scan: %w", err)
		}
		r.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		out = append(out, r)
	}
	return out, rows.Err()
}

// SumTokens returns cumulative (tokens_in, tokens_out) for a model, across
// all providers/keys.
func (s *Store) SumTokens(model string) (in, out int64, err error) {
	err = s.db.QueryRow(`SELECT COALESCE(SUM(tokens_in),0), COALESCE(SUM(tokens_out),0)
		FROM usage_records WHERE model = ?`, model).Scan(&in, &out)
	if err != nil {
		return 0, 0, fmt.Errorf("usage: sum tokens: %w", err)
	}
	return in, out, nil
}
