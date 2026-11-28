package main

// CLI face of the owner decision channel (M6-6 §7): pending / approve /
// deny against a sandbox AGENTVAULT_HOME.

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/zhezhe-star/AgentVault/internal/app"
	"github.com/zhezhe-star/AgentVault/internal/provider"
)

// seedPending writes one open decision directly into the sandbox store.
func seedPending(t *testing.T, home string) int64 {
	t.Helper()
	t.Setenv("AGENTVAULT_HOME", home) // Resolve before any runCLI call
	t.Setenv("AGENTVAULT_SECRET_BACKEND", "memory")
	cfg, err := app.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	store, err := cfg.OpenStore()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	id, err := store.CreatePendingDecision(provider.PendingDecision{
		Provider: "deepseek", Model: "deepseek-chat", TaskTier: "background",
		MatchedRule: "bg-default", Reason: "quota band critical", EstTokens: 4000, EstCost: 0.001,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestCLIAutoDecisionFlow(t *testing.T) {
	home := sandbox(t)
	id := seedPending(t, home)

	// pending lists the open row.
	out := runCLI(t, home, "auto", "pending")
	if !strings.Contains(out, "#"+itoa(id)) || !strings.Contains(out, "background") || !strings.Contains(out, "deepseek/deepseek-chat") {
		t.Fatalf("pending output: %s", out)
	}

	// approve <id>.
	out = runCLI(t, home, "auto", "approve", itoa(id))
	if !strings.Contains(out, "approved") {
		t.Fatalf("approve output: %s", out)
	}
	// Open list is now empty; --all still shows the decided row.
	if out = runCLI(t, home, "auto", "pending"); !strings.Contains(out, "no pending decisions") {
		t.Fatalf("pending after approve: %s", out)
	}
	if out = runCLI(t, home, "auto", "pending", "--all"); !strings.Contains(out, "approved") {
		t.Fatalf("pending --all: %s", out)
	}

	// deny all on a fresh row.
	seedPending(t, home)
	seedPending(t, home)
	out = runCLI(t, home, "auto", "deny", "all")
	if !strings.Contains(out, "2 pending decision(s) denied") {
		t.Fatalf("deny all output: %s", out)
	}
	// Decision events landed in the event file (metadata only).
	raw := readFileT(t, filepath.Join(home, "auto-events.jsonl"))
	if !strings.Contains(raw, "pending_decision_decided") {
		t.Fatalf("event file: %s", raw)
	}
	if strings.Contains(raw, "sk-") || strings.Contains(raw, "api_key") {
		t.Fatalf("event file must stay metadata-only: %s", raw)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func readFileT(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}
