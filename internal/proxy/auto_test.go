package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zhezhe-star/AgentVault/internal/auto"
	"github.com/zhezhe-star/AgentVault/internal/provider"
	"github.com/zhezhe-star/AgentVault/internal/usage"
)

// upstreamEcho is a mock chat-completions upstream capturing the model id
// of each request body.
func upstreamEcho(t *testing.T, seen *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if m, _ := body["model"].(string); m != "" {
			*seen = append(*seen, m)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"chatcmpl-1","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
}

// autoServer builds a proxy whose models table carries the GLM aliases.
func autoServer(t *testing.T, upstreamURL string) (*Server, *usageRecorder) {
	t.Helper()
	store, sec, svc := fixture(t, upstreamURL)
	for _, alias := range []string{"glm-5.4", "glm-5.3-flash"} {
		if err := store.CreateModel(provider.Model{
			ID: "mockprov/k1/" + alias, ProviderID: "mockprov", KeyID: "k1",
			Name: alias, Upstream: alias,
		}); err != nil {
			t.Fatalf("CreateModel %s: %v", alias, err)
		}
	}
	srv := New(store, sec, svc)
	rec := &usageRecorder{}
	srv.SetUsageSink(rec.add)
	return srv, rec
}

// usageRecorder collects usage records for assertions.
type usageRecorder struct {
	records []usage.Record
}

func (u *usageRecorder) add(r usage.Record) { u.records = append(u.records, r) }

// TestAutoEntrypointsRouteToTargetAliases covers M6-2 end to end: entrypoint
// in, concrete GLM alias upstream, decision fields in usage.
func TestAutoEntrypointsRouteToTargetAliases(t *testing.T) {
	var seen []string
	upstream := upstreamEcho(t, &seen)
	defer upstream.Close()
	srv, rec := autoServer(t, upstream.URL)
	base := startProxy(t, srv)

	cases := []struct {
		body       string
		wantModel  string
		wantTier   string
		wantRule   string
		wantManual bool
	}{
		{`{"model":"auto","thinking":{"type":"enabled"},"messages":[{"role":"user","content":"x"}]}`,
			"glm-5.4", "deep", "thinking-on", false},
		{`{"model":"auto","messages":[{"role":"user","content":"x"}]}`,
			"glm-5.3-flash", "routine", "no-tools", false},
		{`{"model":"auto-deep","messages":[{"role":"user","content":"x"}]}`,
			"glm-5.4", "deep", "pinned-deep", false},
		{`{"model":"auto-bg","thinking":{"type":"enabled"},"messages":[{"role":"user","content":"x"}]}`,
			"glm-5.3-flash", "background", "pinned-bg", false},
	}
	for i, tc := range cases {
		resp, err := http.Post(base+"/v1/chat/completions", "application/json", strings.NewReader(tc.body))
		if err != nil {
			t.Fatalf("case %d: post: %v", i, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("case %d: status %d", i, resp.StatusCode)
		}
		resp.Body.Close()
		if len(seen) != i+1 || seen[i] != tc.wantModel {
			t.Fatalf("case %d: upstream saw %v, want %s", i, seen, tc.wantModel)
		}
		got := rec.records[len(rec.records)-1]
		if got.TaskTier != tc.wantTier || got.MatchedRule != tc.wantRule || got.ViaManual != tc.wantManual {
			t.Fatalf("case %d: usage decision fields %+v", i, got)
		}
	}
}

// TestExplicitAliasPassthroughRegression pins the design red line: explicit
// aliases keep exactly the old semantics — model rewritten to the upstream
// id, no decision tier, via_manual tagged.
func TestExplicitAliasPassthroughRegression(t *testing.T) {
	var seen []string
	upstream := upstreamEcho(t, &seen)
	defer upstream.Close()
	srv, rec := autoServer(t, upstream.URL)
	base := startProxy(t, srv)

	resp, err := http.Post(base+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"test-model","messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if len(seen) != 1 || seen[0] != "test-model" {
		t.Fatalf("upstream saw %v, want test-model", seen)
	}
	got := rec.records[len(rec.records)-1]
	if got.ViaManual != true || got.TaskTier != "" || got.MatchedRule != "explicit-alias" {
		t.Fatalf("explicit alias usage fields: %+v", got)
	}

	// Unknown explicit alias still 404s exactly as before.
	if auto.IsEntrypoint("no-such-model") {
		t.Fatal("sanity")
	}
	resp2, err := http.Post(base+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"no-such-model","messages":[]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown alias status %d, want 404", resp2.StatusCode)
	}
}

// TestAutoResponsesEntrypoint: the /v1/responses bridge resolves
// entrypoints too.
func TestAutoResponsesEntrypoint(t *testing.T) {
	var seen []string
	upstream := upstreamEcho(t, &seen)
	defer upstream.Close()
	srv, rec := autoServer(t, upstream.URL)
	base := startProxy(t, srv)

	resp, err := http.Post(base+"/v1/responses", "application/json",
		strings.NewReader(`{"model":"auto-deep","input":[{"type":"message","role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if len(seen) != 1 || seen[0] != "glm-5.4" {
		t.Fatalf("responses bridge upstream saw %v, want glm-5.4", seen)
	}
	got := rec.records[len(rec.records)-1]
	if got.TaskTier != "deep" || got.MatchedRule != "pinned-deep" || got.ViaManual {
		t.Fatalf("responses usage fields: %+v", got)
	}
}

// TestEntrypointKnownNotCandidates: known() treats entrypoints as known
// while candidates() refuses to treat them as routable rows.
func TestEntrypointKnownNotCandidates(t *testing.T) {
	srv, _ := autoServer(t, "http://127.0.0.1:1")
	for _, ep := range auto.EntryPoints {
		if !srv.router.known(ep) {
			t.Fatalf("known(%s) = false", ep)
		}
		if _, err := srv.router.candidates(ep); err == nil {
			t.Fatalf("candidates(%s) must refuse entrypoints", ep)
		}
	}
}
