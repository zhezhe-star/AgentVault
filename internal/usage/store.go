// Package usage persists per-call usage records: one row per proxied
// request with provider/key/model, token counts, latency and status. It
// opens the same SQLite file as the provider store (WAL allows concurrent
// connections) but keeps its own forward-only migration marker.
package usage

import (
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, registers "sqlite"
)

// Record is one proxied call.
type Record struct {
	ProviderID string `json:"provider_id"`
	KeyID      string `json:"key_id"`
	Model      string `json:"model"`
	Protocol   string `json:"protocol"` // "openai" | "anthropic"
	Status     int    `json:"status"`   // final status relayed to the client
	TokensIn   int64  `json:"tokens_in"`
	TokensOut  int64  `json:"tokens_out"`
	LatencyMS  int64  `json:"latency_ms"`
	// Auto-mode decision fields (M6-A, design §11). Empty/false for
	// explicit alias passthrough except ViaManual. Overflow,
	// cache_rebind_cost and off_peak are reserved slots for later blocks
	// (M6-6/M6-4/M6-7) and stay zero for now.
	TaskTier        string    `json:"task_tier,omitempty"`
	MatchedRule     string    `json:"matched_rule,omitempty"`
	DecisionReason  string    `json:"decision_reason,omitempty"`
	ViaManual       bool      `json:"via_manual"`
	Overflow        bool      `json:"overflow"`
	CacheRebindCost int64     `json:"cache_rebind_cost"`
	OffPeak         bool      `json:"off_peak"`
	CreatedAt       time.Time `json:"created_at"`
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
	// M6-A appends the auto-decision columns (forward-only; ALTER is
	// idempotent via the pragma check so existing databases migrate in
	// place without a version table).
	for _, col := range []struct {
		name string
		ddl  string
	}{
		{"task_tier", `ALTER TABLE usage_records ADD COLUMN task_tier TEXT NOT NULL DEFAULT ''`},
		{"matched_rule", `ALTER TABLE usage_records ADD COLUMN matched_rule TEXT NOT NULL DEFAULT ''`},
		{"decision_reason", `ALTER TABLE usage_records ADD COLUMN decision_reason TEXT NOT NULL DEFAULT ''`},
		{"via_manual", `ALTER TABLE usage_records ADD COLUMN via_manual INTEGER NOT NULL DEFAULT 0`},
		{"overflow", `ALTER TABLE usage_records ADD COLUMN overflow INTEGER NOT NULL DEFAULT 0`},
		{"cache_rebind_cost", `ALTER TABLE usage_records ADD COLUMN cache_rebind_cost INTEGER NOT NULL DEFAULT 0`},
		{"off_peak", `ALTER TABLE usage_records ADD COLUMN off_peak INTEGER NOT NULL DEFAULT 0`},
	} {
		exists, err := columnExists(db, "usage_records", col.name)
		if err != nil {
			db.Close()
			return nil, fmt.Errorf("usage: inspect %s: %w", col.name, err)
		}
		if exists {
			continue
		}
		if _, err := db.Exec(col.ddl); err != nil {
			db.Close()
			return nil, fmt.Errorf("usage: add column %s: %w", col.name, err)
		}
	}
	return &Store{db: db}, nil
}

// columnExists reports whether a table has the named column.
func columnExists(db *sql.DB, table, column string) (bool, error) {
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?) WHERE name = ?`, table, column)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	return rows.Next(), rows.Err()
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
		(created_at, provider_id, key_id, model, protocol, status, tokens_in, tokens_out, latency_ms,
		 task_tier, matched_rule, decision_reason, via_manual, overflow, cache_rebind_cost, off_peak)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.CreatedAt.Format(time.RFC3339Nano), r.ProviderID, r.KeyID, r.Model,
		r.Protocol, r.Status, r.TokensIn, r.TokensOut, r.LatencyMS,
		r.TaskTier, r.MatchedRule, r.DecisionReason, boolInt(r.ViaManual),
		boolInt(r.Overflow), r.CacheRebindCost, boolInt(r.OffPeak))
	if err != nil {
		return fmt.Errorf("usage: insert: %w", err)
	}
	return nil
}

// Query returns records ordered newest-first, capped at limit (0 = 100).
func (s *Store) Query(limit int) ([]Record, error) {
	return s.QueryFilter(Filter{Limit: limit})
}

// Filter narrows a usage query. Zero values are ignored.
type Filter struct {
	ProviderID string
	KeyID      string
	Model      string
	Since      time.Time // inclusive
	Until      time.Time // inclusive
	Limit      int
}

// QueryFilter returns filtered records, newest first.
func (s *Store) QueryFilter(f Filter) ([]Record, error) {
	if f.Limit <= 0 {
		f.Limit = 100
	}
	where, args := []string{"1=1"}, []any{}
	if f.ProviderID != "" {
		where, args = append(where, "provider_id = ?"), append(args, f.ProviderID)
	}
	if f.KeyID != "" {
		where, args = append(where, "key_id = ?"), append(args, f.KeyID)
	}
	if f.Model != "" {
		where, args = append(where, "model = ?"), append(args, f.Model)
	}
	if !f.Since.IsZero() {
		// UTC: stored timestamps are UTC, and created_at comparison is a
		// string comparison — a local +08:00 offset would sort wrong.
		where, args = append(where, "created_at >= ?"), append(args, f.Since.UTC().Format(time.RFC3339Nano))
	}
	if !f.Until.IsZero() {
		where, args = append(where, "created_at <= ?"), append(args, f.Until.UTC().Format(time.RFC3339Nano))
	}
	args = append(args, f.Limit)
	rows, err := s.db.Query(`SELECT created_at, provider_id, key_id, model, protocol, status, tokens_in, tokens_out, latency_ms,
		task_tier, matched_rule, decision_reason, via_manual, overflow, cache_rebind_cost, off_peak
		FROM usage_records WHERE `+strings.Join(where, " AND ")+` ORDER BY id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("usage: query: %w", err)
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		var r Record
		var created string
		var viaManual, overflow, offPeak int
		if err := rows.Scan(&created, &r.ProviderID, &r.KeyID, &r.Model, &r.Protocol,
			&r.Status, &r.TokensIn, &r.TokensOut, &r.LatencyMS,
			&r.TaskTier, &r.MatchedRule, &r.DecisionReason, &viaManual, &overflow, &r.CacheRebindCost, &offPeak); err != nil {
			return nil, fmt.Errorf("usage: scan: %w", err)
		}
		r.ViaManual, r.Overflow, r.OffPeak = viaManual == 1, overflow == 1, offPeak == 1
		r.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		out = append(out, r)
	}
	return out, rows.Err()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
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

// SumTokensSince returns (tokens_in, tokens_out) since the given instant.
// providerID / keyID narrow the sum; empty strings mean "all".
func (s *Store) SumTokensSince(providerID, keyID string, since time.Time) (in, out int64, err error) {
	where, args := []string{"created_at >= ?"}, []any{since.UTC().Format(time.RFC3339Nano)}
	if providerID != "" {
		where, args = append(where, "provider_id = ?"), append(args, providerID)
	}
	if keyID != "" {
		where, args = append(where, "key_id = ?"), append(args, keyID)
	}
	err = s.db.QueryRow(`SELECT COALESCE(SUM(tokens_in),0), COALESCE(SUM(tokens_out),0)
		FROM usage_records WHERE `+strings.Join(where, " AND "), args...).Scan(&in, &out)
	if err != nil {
		return 0, 0, fmt.Errorf("usage: sum tokens since: %w", err)
	}
	return in, out, nil
}
