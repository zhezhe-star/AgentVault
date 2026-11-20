package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhezhe-star/AgentVault/internal/app"
	"github.com/zhezhe-star/AgentVault/internal/usage"
)

func usageTestCfg(t *testing.T) (app.Config, *usage.Store) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("AGENTVAULT_HOME", home)
	st, err := usage.Open(filepath.Join(home, "agentvault.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return app.Config{DBPath: filepath.Join(home, "agentvault.db")}, st
}

func TestUsageQueryFilterJSON(t *testing.T) {
	cfg, st := usageTestCfg(t)
	for i := 0; i < 3; i++ {
		if err := st.Insert(usage.Record{ProviderID: "p1", KeyID: "k1", Model: "m1",
			Protocol: "openai", Status: 200, TokensIn: 10, TokensOut: 5}); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	if err := st.Insert(usage.Record{ProviderID: "p2", KeyID: "k9", Model: "m2",
		Protocol: "anthropic", Status: 429}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	out := &bytes.Buffer{}
	cmd := newUsageCmd(cfg, out)
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 5 { // header + 4 rows
		t.Fatalf("expected header+4 rows, got %d", len(lines))
	}
	if !strings.Contains(out.String(), "p2") {
		t.Fatal("missing p2 record")
	}

	// Provider filter must exclude p2 entirely.
	out.Reset()
	cmd = newUsageCmd(cfg, out)
	if err := cmd.Flags().Set("provider", "p1"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("run filtered: %v", err)
	}
	if strings.Contains(out.String(), "p2") || strings.Contains(out.String(), "k9") {
		t.Fatal("provider filter leaked other providers")
	}
	if lines := strings.Split(strings.TrimSpace(out.String()), "\n"); len(lines) != 4 {
		t.Fatalf("filtered rows = %d, want header+3", len(lines))
	}

	// --json output parses and carries the right totals.
	out.Reset()
	cmd = newUsageCmd(cfg, out)
	if err := cmd.Flags().Set("json", "true"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("run json: %v", err)
	}
	var records []usage.Record
	if err := json.Unmarshal(out.Bytes(), &records); err != nil {
		t.Fatalf("decode json: %v", err)
	}
	if len(records) != 4 {
		t.Fatalf("json records = %d, want 4", len(records))
	}
	for _, r := range records {
		if r.ProviderID == "p1" && (r.TokensIn != 10 || r.TokensOut != 5) {
			t.Fatalf("unexpected p1 record: %+v", r)
		}
	}
}

func TestUsageSinceWindow(t *testing.T) {
	cfg, st := usageTestCfg(t)
	if err := st.Insert(usage.Record{ProviderID: "p1", KeyID: "k1", Model: "m1",
		Protocol: "openai", Status: 200, TokensIn: 1}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	out := &bytes.Buffer{}
	cmd := newUsageCmd(cfg, out)
	if err := cmd.Flags().Set("since", "1h"); err != nil { // record just inserted is inside the window
		t.Fatal(err)
	}
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out.String(), "m1") {
		t.Fatal("since window excluded a fresh record")
	}

	// An until in the past excludes it.
	out.Reset()
	cmd = newUsageCmd(cfg, out)
	if err := cmd.Flags().Set("until", time.Now().Add(-1*time.Hour).Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out.String(), "no usage records match") {
		t.Fatalf("past window should be empty: %s", out.String())
	}
}
