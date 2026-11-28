package proxy

// M6-C tests: owner decision channel (§7), off-peak windows (§9) and the
// observability face (§11). All external interaction goes through
// httptest mock upstreams; the DeepSeek probe is a stub.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhezhe-star/AgentVault/internal/auto"
	"github.com/zhezhe-star/AgentVault/internal/provider"
)

// m6cServer builds an auto-mode proxy with the M6-C knobs wired: a quota
// source pinned to pct, a DeepSeek provider row (when withDeepSeek) and an
// event file in a temp dir.
func m6cServer(t *testing.T, upstreamURL string, pct float64, withDeepSeek bool) (*Server, *usageRecorder, string) {
	t.Helper()
	srv, rec := autoServer(t, upstreamURL)
	srv.SetQuotaSource(func() float64 { return pct })
	evtPath := filepath.Join(t.TempDir(), "events.jsonl")
	srv.SetEventLog(auto.NewEventLog(evtPath))
	srv.SetDeepSeekProvider("deepseek")
	if withDeepSeek {
		if err := srv.store.CreateProvider(provider.Provider{ID: "deepseek", Name: "DeepSeek", BaseURL: "http://127.0.0.1:1"}); err != nil {
			t.Fatalf("CreateProvider deepseek: %v", err)
		}
		if err := srv.store.CreateKey(provider.Key{ID: "ds1", ProviderID: "deepseek", SecretID: "ref", Status: provider.KeyActive}); err != nil {
			t.Fatalf("CreateKey deepseek: %v", err)
		}
	}
	return srv, rec, evtPath
}

func postModel(t *testing.T, base, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(base+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	return resp
}

func headerGet(t *testing.T, resp *http.Response, name string) string {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	return resp.Header.Get(name)
}

// TestOverflowCandidateFiling covers §7: elevated+background files a
// pending_decision with an event line; a second request dedupes; deciding
// frees the class; deep never files; deepseek-unavailable suppresses.
func TestOverflowCandidateFiling(t *testing.T) {
	var seen []string
	upstream := upstreamEcho(t, &seen)
	defer upstream.Close()
	srv, rec, evtPath := m6cServer(t, upstream.URL, 96, true) // critical band
	base := startProxy(t, srv)

	bg := `{"model":"auto-bg","messages":[{"role":"user","content":"x"}]}`

	// 1) Critical + background files a candidate.
	resp := postModel(t, base, bg)
	dec := headerGet(t, resp, "x-agentvault-decision")
	if !strings.Contains(dec, "/overflow-candidate") {
		t.Fatalf("decision header %q lacks /overflow-candidate", dec)
	}
	open, err := srv.store.ListPendingDecisions(provider.PendingOpen)
	if err != nil || len(open) != 1 {
		t.Fatalf("open pending: %v (n=%d)", err, len(open))
	}
	if open[0].Provider != "deepseek" || open[0].Model != "deepseek-chat" || open[0].TaskTier != "background" {
		t.Fatalf("pending row fields: %+v", open[0])
	}
	if open[0].EstTokens <= 0 || open[0].EstCost <= 0 {
		t.Errorf("estimates should be positive: %+v", open[0])
	}
	rec.last(t, 1) // usage sink fired; keeps ordering

	// 2) Event file line (metadata only).
	raw, err := os.ReadFile(evtPath)
	if err != nil || !strings.Contains(string(raw), auto.EventPendingCreated) {
		t.Fatalf("event file: %v %s", err, raw)
	}
	var ev map[string]any
	if err := json.Unmarshal(raw[:len(raw)-1], &ev); err != nil {
		t.Fatalf("event json: %v", err)
	}
	if _, has := ev["ts"]; !has {
		t.Error("event should carry ts")
	}
	for _, forbidden := range []string{"key", "body", "api_key"} {
		if _, has := ev[forbidden]; has {
			t.Errorf("event must not carry %q", forbidden)
		}
	}

	// 3) Second request dedupes (same open class).
	resp = postModel(t, base, bg)
	dec = headerGet(t, resp, "x-agentvault-decision")
	if strings.Contains(dec, "/overflow-candidate") {
		t.Fatalf("second request should dedupe, header %q", dec)
	}
	if !strings.Contains(rec.last(t, 2).DecisionReason, "open (awaiting owner)") {
		t.Fatalf("dedupe reason: %q", rec.last(t, 2).DecisionReason)
	}
	if got := mustOpen(t, srv); got != 1 {
		t.Fatalf("open count after dedupe = %d, want 1", got)
	}

	// 4) Deciding frees the class; the next request files a new candidate.
	if err := srv.store.DecidePending(open[0].ID, provider.PendingApproved); err != nil {
		t.Fatal(err)
	}
	resp = postModel(t, base, bg)
	if dec = headerGet(t, resp, "x-agentvault-decision"); !strings.Contains(dec, "/overflow-candidate") {
		t.Fatalf("post-decision filing missing, header %q", dec)
	}

	// 5) Deep never files, even critical.
	deep := `{"model":"auto","thinking":{"type":"enabled"},"messages":[{"role":"user","content":"x"}]}`
	// First decide the fresh candidate away so counts stay simple.
	if _, err := srv.store.DecideAllPending(provider.PendingDenied); err != nil {
		t.Fatal(err)
	}
	resp = postModel(t, base, deep)
	dec = headerGet(t, resp, "x-agentvault-decision")
	if !strings.HasPrefix(dec, "deep:") {
		t.Fatalf("deep decision header %q", dec)
	}
	if mustOpen(t, srv) != 0 {
		t.Error("deep must never file a pending decision")
	}
}

func mustOpen(t *testing.T, s *Server) int {
	t.Helper()
	n, err := s.store.CountOpenPending()
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// TestOverflowBandEligibility: routine only files at critical; elevated
// only admits background; deepseek-unavailable degrades to a reason note.
func TestOverflowBandEligibility(t *testing.T) {
	t.Run("routine at elevated does not file", func(t *testing.T) {
		var seen []string
		upstream := upstreamEcho(t, &seen)
		defer upstream.Close()
		srv, _, _ := m6cServer(t, upstream.URL, 75, true)
		base := startProxy(t, srv)
		resp := postModel(t, base, `{"model":"auto","messages":[{"role":"user","content":"x"}]}`)
		headerGet(t, resp, "x-agentvault-decision")
		if got := mustOpen(t, srv); got != 0 {
			t.Fatalf("routine at elevated filed %d candidates, want 0", got)
		}
	})
	t.Run("routine at critical files", func(t *testing.T) {
		var seen []string
		upstream := upstreamEcho(t, &seen)
		defer upstream.Close()
		srv, _, _ := m6cServer(t, upstream.URL, 96, true)
		base := startProxy(t, srv)
		resp := postModel(t, base, `{"model":"auto","messages":[{"role":"user","content":"x"}]}`)
		headerGet(t, resp, "x-agentvault-decision")
		if got := mustOpen(t, srv); got != 1 {
			t.Fatalf("routine at critical filed %d, want 1", got)
		}
	})
	t.Run("healthy band never files", func(t *testing.T) {
		var seen []string
		upstream := upstreamEcho(t, &seen)
		defer upstream.Close()
		srv, _, _ := m6cServer(t, upstream.URL, 10, true)
		base := startProxy(t, srv)
		resp := postModel(t, base, `{"model":"auto-bg","messages":[{"role":"user","content":"x"}]}`)
		headerGet(t, resp, "x-agentvault-decision")
		if got := mustOpen(t, srv); got != 0 {
			t.Fatalf("healthy filed %d, want 0", got)
		}
	})
	t.Run("deepseek unavailable suppresses with reason", func(t *testing.T) {
		var seen []string
		upstream := upstreamEcho(t, &seen)
		defer upstream.Close()
		srv, rec, _ := m6cServer(t, upstream.URL, 96, false) // no provider row
		base := startProxy(t, srv)
		resp := postModel(t, base, `{"model":"auto-bg","messages":[{"role":"user","content":"x"}]}`)
		headerGet(t, resp, "x-agentvault-decision")
		reason := rec.last(t, 1).DecisionReason
		if !strings.Contains(reason, "suppressed: deepseek unavailable") {
			t.Fatalf("reason %q lacks suppression note", reason)
		}
		if got := mustOpen(t, srv); got != 0 {
			t.Fatalf("unavailable deepseek filed %d, want 0", got)
		}
	})
	t.Run("probe reporting unavailable suppresses", func(t *testing.T) {
		var seen []string
		upstream := upstreamEcho(t, &seen)
		defer upstream.Close()
		srv, _, _ := m6cServer(t, upstream.URL, 96, true)
		srv.SetDeepSeekStatus(func() (auto.DeepSeekStatus, bool) {
			return auto.DeepSeekStatus{Available: false, CheckedAt: time.Now()}, true
		})
		base := startProxy(t, srv)
		resp := postModel(t, base, `{"model":"auto-bg","messages":[{"role":"user","content":"x"}]}`)
		headerGet(t, resp, "x-agentvault-decision")
		if got := mustOpen(t, srv); got != 0 {
			t.Fatalf("probe-unavailable filed %d, want 0", got)
		}
	})
}

// TestObservabilityHeaders covers §11: the three auto headers on entrypoint
// requests, route-only on explicit aliases.
func TestObservabilityHeaders(t *testing.T) {
	var seen []string
	upstream := upstreamEcho(t, &seen)
	defer upstream.Close()
	srv, _, _ := m6cServer(t, upstream.URL, 50, true)
	// Force off-peak: declare the peak window to start 3 hours from now for
	// one hour — now can never be inside it (even across midnight).
	h := (time.Now().Hour() + 3) % 24
	srv.SetOffPeakWindows(
		auto.PeakWindows{Loc: time.UTC, Windows: []auto.Window{{StartHour: h, EndHour: (h + 1) % 24}}},
		auto.PeakWindows{Loc: time.UTC, Windows: []auto.Window{{StartHour: h, EndHour: (h + 1) % 24}}})
	base := startProxy(t, srv)

	resp := postModel(t, base, `{"model":"auto","messages":[{"role":"user","content":"x"}]}`)
	route := headerGet(t, resp, "x-agentvault-route")
	if route != "mockprov/glm-5.3-flash" {
		t.Errorf("route header = %q, want mockprov/glm-5.3-flash", route)
	}
	dec := headerGet(t, resp, "x-agentvault-decision")
	if !strings.HasPrefix(dec, "routine:") {
		t.Errorf("decision header = %q, want routine:<rule>", dec)
	}
	if op := headerGet(t, resp, "x-agentvault-off-peak"); op != "true" {
		t.Errorf("off-peak header = %q, want true (outside declared peak)", op)
	}
	// Usage record carries off_peak too.
	// (checked below via a fresh explicit request instead — see off-peak usage test)

	// Explicit alias: only the route header.
	resp = postModel(t, base, `{"model":"test-model","messages":[{"role":"user","content":"x"}]}`)
	route = headerGet(t, resp, "x-agentvault-route")
	if route != "mockprov/test-model" {
		t.Errorf("explicit route header = %q", route)
	}
	if v := resp.Header.Get("x-agentvault-decision"); v != "" {
		t.Errorf("explicit alias should not carry decision header, got %q", v)
	}
	if v := resp.Header.Get("x-agentvault-off-peak"); v != "" {
		t.Errorf("explicit alias should not carry off-peak header, got %q", v)
	}
}

// TestOffPeakUsageStamp: inside off-peak, usage records off_peak=true.
func TestOffPeakUsageStamp(t *testing.T) {
	var seen []string
	upstream := upstreamEcho(t, &seen)
	defer upstream.Close()
	srv, rec, _ := m6cServer(t, upstream.URL, 50, true)
	h := (time.Now().Hour() + 3) % 24
	srv.SetOffPeakWindows(
		auto.PeakWindows{Loc: time.UTC, Windows: []auto.Window{{StartHour: h, EndHour: (h + 1) % 24}}},
		auto.PeakWindows{})
	base := startProxy(t, srv)
	resp := postModel(t, base, `{"model":"auto","messages":[{"role":"user","content":"x"}]}`)
	headerGet(t, resp, "x-agentvault-off-peak")
	got := rec.last(t, 1)
	if !got.OffPeak {
		t.Errorf("usage off_peak = false, want true")
	}
	if !strings.Contains(got.DecisionReason, "off-peak window") {
		t.Errorf("reason %q lacks off-peak note", got.DecisionReason)
	}
}

// TestRoutingAutoPanel: /admin/routing exposes the §11 auto panel.
func TestRoutingAutoPanel(t *testing.T) {
	var seen []string
	upstream := upstreamEcho(t, &seen)
	defer upstream.Close()
	srv, _, _ := m6cServer(t, upstream.URL, 96, true)
	base := startProxy(t, srv)
	for i := 0; i < 2; i++ {
		resp := postModel(t, base, `{"model":"auto-bg","messages":[{"role":"user","content":"x"}]}`)
		headerGet(t, resp, "x-agentvault-decision")
	}
	resp, err := http.Get(base + "/admin/routing")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Auto struct {
			Requests           map[string]int64 `json:"requests"`
			OverflowCandidates int64            `json:"overflow_candidates"`
			Pending            int              `json:"pending"`
		} `json:"auto"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Auto.Requests["background"] != 2 {
		t.Errorf("background requests = %d, want 2", out.Auto.Requests["background"])
	}
	if out.Auto.OverflowCandidates < 1 {
		t.Errorf("overflow_candidates = %d, want >= 1", out.Auto.OverflowCandidates)
	}
	if out.Auto.Pending != 1 {
		t.Errorf("pending = %d, want 1 (deduped)", out.Auto.Pending)
	}
}

// TestConsolePendingAPI: the console's third decision face lists and
// decides candidates.
func TestConsolePendingAPI(t *testing.T) {
	var seen []string
	upstream := upstreamEcho(t, &seen)
	defer upstream.Close()
	srv, _, _ := m6cServer(t, upstream.URL, 96, true)
	base := startProxy(t, srv)
	resp := postModel(t, base, `{"model":"auto-bg","messages":[{"role":"user","content":"x"}]}`)
	headerGet(t, resp, "x-agentvault-decision")

	// GET lists the open candidate.
	get, err := http.Get(base + "/console/api/pending")
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Pending []provider.PendingDecision `json:"pending"`
	}
	if err := json.NewDecoder(get.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	get.Body.Close()
	if len(list.Pending) != 1 {
		t.Fatalf("console pending list = %d, want 1", len(list.Pending))
	}

	// POST deny by id.
	id := list.Pending[0].ID
	post, err := http.Post(base+"/console/api/pending", "application/json",
		strings.NewReader(fmt.Sprintf(`{"decision":"deny","id":%d}`, id)))
	if err != nil {
		t.Fatal(err)
	}
	post.Body.Close()
	if post.StatusCode != http.StatusOK {
		t.Fatalf("decide status %d", post.StatusCode)
	}
	if got := mustOpen(t, srv); got != 0 {
		t.Fatalf("after console deny open = %d, want 0", got)
	}
	// Row shows up under the all-view via status denied.
	denied, _ := srv.store.ListPendingDecisions(provider.PendingDenied)
	if len(denied) != 1 {
		t.Fatalf("denied list = %d, want 1", len(denied))
	}

	// Bad decision body is a 400.
	bad, err := http.Post(base+"/console/api/pending", "application/json",
		strings.NewReader(`{"decision":"maybe"}`))
	if err != nil {
		t.Fatal(err)
	}
	bad.Body.Close()
	if bad.StatusCode != http.StatusBadRequest {
		t.Errorf("bad decision status %d, want 400", bad.StatusCode)
	}
}

// TestOffPeakBoostEffectiveLimit: the router's off-peak hook raises the
// gate's effective limit only when installed and only for the boosted
// provider (§9 闲时放宽).
func TestOffPeakBoostEffectiveLimit(t *testing.T) {
	var seen []string
	upstream := upstreamEcho(t, &seen)
	defer upstream.Close()
	srv, _, _ := m6cServer(t, upstream.URL, 50, true)

	c := candidate{providerID: "mockprov", limit: 4}
	if got := srv.router.effectiveLimit(c); got != 4 {
		t.Fatalf("no boost: limit = %d, want 4", got)
	}
	srv.SetOffPeakBoost(func(providerID string, limit int) int {
		if providerID == "mockprov" {
			return int(float64(limit) * 1.5)
		}
		return limit
	})
	if got := srv.router.effectiveLimit(c); got != 6 {
		t.Errorf("boosted limit = %d, want 6", got)
	}
	other := candidate{providerID: "deepseek", limit: 4}
	if got := srv.router.effectiveLimit(other); got != 4 {
		t.Errorf("non-boosted provider limit = %d, want 4", got)
	}
	unlimited := candidate{providerID: "mockprov", limit: 0}
	if got := srv.router.effectiveLimit(unlimited); got != 0 {
		t.Errorf("unlimited must stay 0, got %d", got)
	}
}
