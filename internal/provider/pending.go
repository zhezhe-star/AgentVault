package provider

// Owner decision channel store (auto mode M6-6, design §7): the
// pending_decision table backing `agentvault auto pending/approve/deny`,
// the console card and the proxy's overflow-candidate filing. All values
// are request metadata; secrets never enter here.

import (
	"database/sql"
	"fmt"
	"time"
)

// Pending decision statuses.
const (
	PendingOpen     = "pending"
	PendingApproved = "approved"
	PendingDenied   = "denied"
)

// PendingDecision is one overflow candidate awaiting the owner.
type PendingDecision struct {
	ID          int64     `json:"id"`
	Provider    string    `json:"provider"`  // overflow target provider (e.g. deepseek)
	Model       string    `json:"model"`     // overflow target model (e.g. deepseek-chat)
	TaskTier    string    `json:"task_tier"` // background | routine (deep never files)
	MatchedRule string    `json:"matched_rule"`
	Reason      string    `json:"reason"`
	EstTokens   int64     `json:"est_tokens"`
	EstCost     float64   `json:"est_cost"` // rough USD estimate for display; not billing
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
	DecidedAt   time.Time `json:"decided_at"` // zero while open
}

func scanPending(row interface{ Scan(...any) error }) (PendingDecision, error) {
	var d PendingDecision
	var created, decided string
	if err := row.Scan(&d.ID, &d.Provider, &d.Model, &d.TaskTier, &d.MatchedRule,
		&d.Reason, &d.EstTokens, &d.EstCost, &d.Status, &created, &decided); err != nil {
		return d, err
	}
	if t, err := time.Parse(time.RFC3339, created); err == nil {
		d.CreatedAt = t
	}
	if t, err := time.Parse(time.RFC3339, decided); err == nil {
		d.DecidedAt = t
	}
	return d, nil
}

const pendingCols = `id, provider, model, task_tier, matched_rule, reason, est_tokens, est_cost, status, created_at, decided_at`

// CreatePendingDecision files one overflow candidate and returns its row id.
func (s *Store) CreatePendingDecision(d PendingDecision) (int64, error) {
	if d.Status == "" {
		d.Status = PendingOpen
	}
	now := time.Now().UTC()
	if d.CreatedAt.IsZero() {
		d.CreatedAt = now
	}
	res, err := s.db.Exec(`INSERT INTO pending_decision
		(provider, model, task_tier, matched_rule, reason, est_tokens, est_cost, status, created_at, decided_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		d.Provider, d.Model, d.TaskTier, d.MatchedRule, d.Reason,
		d.EstTokens, d.EstCost, d.Status,
		d.CreatedAt.Format(time.RFC3339),
		d.DecidedAt.Format(time.RFC3339))
	if err != nil {
		return 0, fmt.Errorf("provider: create pending decision: %w", err)
	}
	return res.LastInsertId()
}

// ListPendingDecisions returns decisions with the given status, newest
// first; status "" means all.
func (s *Store) ListPendingDecisions(status string) ([]PendingDecision, error) {
	q := `SELECT ` + pendingCols + ` FROM pending_decision`
	args := []any{}
	if status != "" {
		q += ` WHERE status = ?`
		args = append(args, status)
	}
	q += ` ORDER BY id DESC`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("provider: list pending decisions: %w", err)
	}
	defer rows.Close()
	var out []PendingDecision
	for rows.Next() {
		d, err := scanPending(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// GetPendingDecision reads one decision by id.
func (s *Store) GetPendingDecision(id int64) (PendingDecision, error) {
	d, err := scanPending(s.db.QueryRow(`SELECT `+pendingCols+` FROM pending_decision WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return d, ErrNotFound
	}
	return d, err
}

// HasOpenPending reports whether an open candidate with the same tier,
// provider and model already exists (dedupe: the soft quota band fires on
// every request while elevated, but one open row per candidate class is
// enough for the owner to act on).
func (s *Store) HasOpenPending(taskTier, providerID, model string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM pending_decision
		WHERE status = ? AND task_tier = ? AND provider = ? AND model = ?`,
		PendingOpen, taskTier, providerID, model).Scan(&n)
	return n > 0, err
}

// DecidePending sets the status of one open decision (no-op when the row
// is already decided or missing).
func (s *Store) DecidePending(id int64, status string) error {
	if status != PendingApproved && status != PendingDenied {
		return fmt.Errorf("provider: bad pending status %q", status)
	}
	res, err := s.db.Exec(`UPDATE pending_decision
		SET status = ?, decided_at = ? WHERE id = ? AND status = ?`,
		status, time.Now().UTC().Format(time.RFC3339), id, PendingOpen)
	if err != nil {
		return fmt.Errorf("provider: decide pending %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DecideAllPending sets the status of every open decision and returns how
// many rows changed (the `approve|deny all` bulk path).
func (s *Store) DecideAllPending(status string) (int64, error) {
	if status != PendingApproved && status != PendingDenied {
		return 0, fmt.Errorf("provider: bad pending status %q", status)
	}
	res, err := s.db.Exec(`UPDATE pending_decision
		SET status = ?, decided_at = ? WHERE status = ?`,
		status, time.Now().UTC().Format(time.RFC3339), PendingOpen)
	if err != nil {
		return 0, fmt.Errorf("provider: decide all pending: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// CountOpenPending reports how many decisions await the owner (console /
// /admin/routing auto panel).
func (s *Store) CountOpenPending() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM pending_decision WHERE status = ?`, PendingOpen).Scan(&n)
	return n, err
}
