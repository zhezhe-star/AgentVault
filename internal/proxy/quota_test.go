package proxy

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhezhe-star/AgentVault/internal/provider"
	"github.com/zhezhe-star/AgentVault/internal/usage"
)

// quotaFixture wires an anthropic provider whose usage counts come from a
// real usage store in a temp dir.
func quotaFixture(t *testing.T) (*Server, *usage.Store) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"type":"message","usage":{"input_tokens":10,"output_tokens":5}}`))
	}))
	t.Cleanup(upstream.Close)
	store, sec, svc := fixtureAnthropic(t, upstream.URL)
	ustore, err := usage.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatalf("usage open: %v", err)
	}
	t.Cleanup(func() { ustore.Close() })
	srv := New(store, sec, svc)
	srv.EnableQuotas(store, ustore)
	srv.SetUsageSink(func(r usage.Record) { _ = ustore.Insert(r) })
	return srv, ustore
}

func postMessages(t *testing.T, base string) int {
	t.Helper()
	resp, err := http.Post(base+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"anthro-model","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func TestQuotaPerKeyExhausted(t *testing.T) {
	srv, ustore := quotaFixture(t)
	// 15 tokens per call (10+5); cap the key at 30 → two calls pass, third 429s.
	quotas, _ := srv.store.ListQuotas()
	_ = quotas
	if err := srv.store.SetQuota(providerQuotaKey("ka", 30, 3600)); err != nil {
		t.Fatalf("set quota: %v", err)
	}
	base := startProxy(t, srv)

	if got := postMessages(t, base); got != 200 {
		t.Fatalf("call 1 status = %d, want 200", got)
	}
	if got := postMessages(t, base); got != 200 {
		t.Fatalf("call 2 status = %d, want 200", got)
	}
	if got := postMessages(t, base); got != http.StatusTooManyRequests {
		t.Fatalf("call 3 status = %d, want 429", got)
	}
	// Counted history matches the limit independently.
	in, out, _ := ustore.SumTokensSince("mockanthropic", "ka", time.Now().Add(-1*time.Hour))
	if in+out != 30 {
		t.Fatalf("recorded tokens = %d, want 30", in+out)
	}
}

func TestQuotaPerProviderIndependent(t *testing.T) {
	srv, ustore := quotaFixture(t)
	// Provider-level cap 15: each call costs 15, so call 1 passes (sum 0)
	// and call 2 is rejected because the window is already at the cap.
	if err := srv.store.SetQuota(providerQuotaProvider("mockanthropic", 15, 3600)); err != nil {
		t.Fatalf("set quota: %v", err)
	}
	base := startProxy(t, srv)
	if got := postMessages(t, base); got != 200 {
		t.Fatalf("call 1 status = %d, want 200", got)
	}
	if got := postMessages(t, base); got != http.StatusTooManyRequests {
		t.Fatalf("call 2 status = %d, want 429", got)
	}
	_ = ustore
}

func TestQuotaWindowRollsOver(t *testing.T) {
	srv, _ := quotaFixture(t)
	// Tiny window: 15 tokens / 1 second. One call fills it; after the
	// window rolls the quota is available again.
	if err := srv.store.SetQuota(providerQuotaKey("ka", 15, 1)); err != nil {
		t.Fatalf("set quota: %v", err)
	}
	base := startProxy(t, srv)
	if got := postMessages(t, base); got != 200 {
		t.Fatalf("call 1 status = %d", got)
	}
	if got := postMessages(t, base); got != http.StatusTooManyRequests {
		t.Fatalf("call 2 status = %d, want 429", got)
	}
	time.Sleep(1100 * time.Millisecond)
	if got := postMessages(t, base); got != 200 {
		t.Fatalf("post-window call status = %d, want 200", got)
	}
}

func TestQuotaErrorBody(t *testing.T) {
	srv, ustore := quotaFixture(t)
	// Pre-load the window to its cap so the very first call is rejected.
	_ = ustore.Insert(usage.Record{ProviderID: "mockanthropic", KeyID: "ka",
		Model: "anthro-model", Protocol: "anthropic", Status: 200,
		TokensIn: 60, TokensOut: 40})
	if err := srv.store.SetQuota(providerQuotaKey("ka", 100, 3600)); err != nil {
		t.Fatalf("set quota: %v", err)
	}
	base := startProxy(t, srv)
	resp, err := http.Post(base+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"anthro-model","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	body := string(buf[:n])
	if resp.StatusCode != http.StatusTooManyRequests ||
		!strings.Contains(body, "agentvault_quota_exceeded") ||
		!strings.Contains(body, `"scope":"key"`) {
		t.Fatalf("unexpected quota error: status=%d body=%s", resp.StatusCode, body)
	}
	if strings.Contains(body, testKey) {
		t.Fatal("quota error leaked key")
	}
}

// helpers building quota structs without importing provider everywhere.
func providerQuotaKey(keyID string, max int64, windowSec int) provider.Quota {
	return provider.Quota{Scope: "key", RefID: keyID, WindowSeconds: windowSec, MaxTokens: max}
}

func providerQuotaProvider(providerID string, max int64, windowSec int) provider.Quota {
	return provider.Quota{Scope: "provider", RefID: providerID, WindowSeconds: windowSec, MaxTokens: max}
}
