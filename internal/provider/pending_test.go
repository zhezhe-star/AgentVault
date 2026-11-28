package provider

import (
	"path/filepath"
	"testing"
)

// pendingFixture opens a throwaway store for the pending_decision tests.
func pendingFixture(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "pending.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestPendingDecisionLifecycle(t *testing.T) {
	s := pendingFixture(t)

	id1, err := s.CreatePendingDecision(PendingDecision{
		Provider: "deepseek", Model: "deepseek-chat", TaskTier: "background",
		MatchedRule: "bg-default", Reason: "quota band elevated", EstTokens: 4000, EstCost: 0.001,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	id2, err := s.CreatePendingDecision(PendingDecision{
		Provider: "deepseek", Model: "deepseek-chat", TaskTier: "routine",
	})
	if err != nil {
		t.Fatalf("create 2: %v", err)
	}

	// Defaults: status pending, created_at stamped.
	d, err := s.GetPendingDecision(id1)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if d.Status != PendingOpen {
		t.Errorf("status = %q, want pending", d.Status)
	}
	if d.CreatedAt.IsZero() {
		t.Error("created_at should be stamped on create")
	}
	if !d.DecidedAt.IsZero() {
		t.Error("decided_at should be zero while open")
	}

	// Open listing is newest first and excludes decided rows after decision.
	open, err := s.ListPendingDecisions(PendingOpen)
	if err != nil || len(open) != 2 {
		t.Fatalf("list open: %v (n=%d)", err, len(open))
	}
	if open[0].ID != id2 {
		t.Errorf("open[0].ID = %d, want newest %d", open[0].ID, id2)
	}

	// Deny one; decide is only legal from the open state.
	if err := s.DecidePending(id1, PendingDenied); err != nil {
		t.Fatalf("deny: %v", err)
	}
	if err := s.DecidePending(id1, PendingApproved); err != ErrNotFound {
		t.Errorf("re-decide should be NotFound, got %v", err)
	}
	if got, _ := s.CountOpenPending(); got != 1 {
		t.Errorf("open count = %d, want 1", got)
	}

	// Bulk approve the rest.
	n, err := s.DecideAllPending(PendingApproved)
	if err != nil || n != 1 {
		t.Fatalf("decide all: n=%d err=%v", n, err)
	}
	if got, _ := s.CountOpenPending(); got != 0 {
		t.Errorf("open count after all = %d, want 0", got)
	}
	if _, err := s.GetPendingDecision(999); err != ErrNotFound {
		t.Errorf("missing id should be NotFound, got %v", err)
	}
}

func TestHasOpenPendingDedupe(t *testing.T) {
	s := pendingFixture(t)
	id, err := s.CreatePendingDecision(PendingDecision{
		Provider: "deepseek", Model: "deepseek-chat", TaskTier: "background",
	})
	if err != nil {
		t.Fatal(err)
	}
	same, err := s.HasOpenPending("background", "deepseek", "deepseek-chat")
	if err != nil || !same {
		t.Fatalf("HasOpenPending same class = %v, %v; want true", same, err)
	}
	// Different tier / provider / model classes don't dedupe.
	for _, tc := range [][3]string{
		{"routine", "deepseek", "deepseek-chat"},
		{"background", "bigmodel", "glm-5.4"},
		{"background", "deepseek", "deepseek-reasoner"},
	} {
		ok, err := s.HasOpenPending(tc[0], tc[1], tc[2])
		if err != nil || ok {
			t.Errorf("HasOpenPending %v = %v, %v; want false", tc, ok, err)
		}
	}
	// After deciding, the class frees up again (a new candidate may file).
	if err := s.DecidePending(id, PendingApproved); err != nil {
		t.Fatal(err)
	}
	ok, err := s.HasOpenPending("background", "deepseek", "deepseek-chat")
	if err != nil || ok {
		t.Errorf("after decision HasOpenPending = %v, %v; want false", ok, err)
	}
}
