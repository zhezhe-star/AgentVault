package auto

// Tests for the M6-3 quota-awareness clients and poller. Red line: every
// external endpoint here is an httptest fake — the real
// open.bigmodel.cn / api.deepseek.com URLs are never contacted from tests.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestGLMQuotaClientFetch covers the tolerant parser: flat and {"data":…}
// wrappers, 0–1 fractions, header passthrough and error status.
func TestGLMQuotaClientFetch(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		switch r.Header.Get("X-Shape") {
		case "error":
			w.WriteHeader(http.StatusUnauthorized)
			return
		case "wrapped":
			w.Write([]byte(`{"data":{"percentage":0.42,"next_reset_time":1700000000000}}`))
			return
		default:
			w.Write([]byte(`{"percentage":83.5,"next_reset_time":1700000000000}`))
		}
	}))
	defer srv.Close()

	key := func() (string, error) { return "test-key-not-real", nil }

	c := &GLMQuotaClient{BaseURL: srv.URL, Key: key}
	snap, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("fetch flat: %v", err)
	}
	if snap.UsedPct != 83.5 || snap.Source != "api" {
		t.Fatalf("flat snap: %+v", snap)
	}
	if snap.NextReset.UnixMilli() != 1700000000000 {
		t.Fatalf("next reset: %v", snap.NextReset)
	}
	if gotAuth != "test-key-not-real" {
		t.Fatalf("plan key must be the raw Authorization header, got %q", gotAuth)
	}

	c = &GLMQuotaClient{BaseURL: srv.URL, Key: key, HTTP: &http.Client{
		Transport: headerTransport{base: http.DefaultTransport, name: "X-Shape", value: "wrapped"},
	}}
	snap, err = c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("fetch wrapped: %v", err)
	}
	if snap.UsedPct != 42 {
		t.Fatalf("0-1 fraction must scale to percent, got %v", snap.UsedPct)
	}

	c = &GLMQuotaClient{BaseURL: srv.URL, Key: key, HTTP: &http.Client{
		Transport: headerTransport{base: http.DefaultTransport, name: "X-Shape", value: "error"},
	}}
	if _, err := c.Fetch(context.Background()); err == nil {
		t.Fatal("non-200 must error so the poller degrades to the file fallback")
	}
}

type headerTransport struct {
	base  http.RoundTripper
	name  string
	value string
}

func (t headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r.Header.Set(t.name, t.value)
	return t.base.RoundTrip(r)
}

// TestLoadQuotaFile reads the owner's fallback snapshot: unit=5 (5h) and
// unit=6 (weekly) are kept, others skipped.
func TestLoadQuotaFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "glm-quota.json")
	doc := `{"checkedAt":1,"level":"max","limits":[
		{"type":"TIME_LIMIT","unit":5,"usagePct":30,"nextResetTime":1700000000000},
		{"type":"TOKENS_LIMIT","unit":6,"usagePct":72.5,"nextResetTime":1700100000000},
		{"type":"OTHER","unit":3,"usagePct":99}]}`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	snaps, err := LoadQuotaFile(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(snaps) != 2 {
		t.Fatalf("want 5h+weekly snapshots, got %+v", snaps)
	}
	if snaps[0].WindowMinutes != 300 || snaps[0].Source != "file" || snaps[0].UsedPct != 30 {
		t.Fatalf("5h snap: %+v", snaps[0])
	}
	if snaps[1].WindowMinutes != 7*24*60 || snaps[1].UsedPct != 72.5 {
		t.Fatalf("weekly snap: %+v", snaps[1])
	}
	if _, err := LoadQuotaFile(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Fatal("missing file must error")
	}
}

// TestQuotaPollerFallsBackToFile: when the primary API fails, the poller
// must degrade to the fallback file and keep polling (design R1).
func TestQuotaPollerFallsBackToFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "glm-quota.json")
	if err := os.WriteFile(path, []byte(
		`{"limits":[{"type":"TOKENS_LIMIT","unit":6,"usagePct":64}]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	applied := 0
	poller := &QuotaPoller{
		Client:   &GLMQuotaClient{BaseURL: srv.URL, Key: func() (string, error) { return "k", nil }},
		Fallback: path,
		Interval: 5 * time.Millisecond,
		Apply: func(s QuotaSnapshot) error {
			if s.Source != "file" || s.UsedPct != 64 || s.WindowMinutes != 7*24*60 {
				t.Errorf("fallback snapshot: %+v", s)
			}
			mu.Lock()
			applied++
			mu.Unlock()
			return nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- poller.Run(ctx) }()
	// The first read is immediate; give it a beat then stop before the
	// capped backoff window gets long.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := applied
		mu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	// Run returns nil here: the API failure degraded cleanly to the file
	// fallback, so the last *outcome* was a success (R1 — degrades, never
	// fails). The applied snapshot below is the actual degradation proof.
	if err := <-done; err != nil {
		t.Fatalf("clean degradation must not fail the poller: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if applied < 1 {
		t.Fatal("fallback never applied")
	}
}

// TestQuotaPollerSuccess pins the happy path: a working API applies its
// snapshot directly (source=api) with no fallback involvement.
func TestQuotaPollerSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"percentage":10}`))
	}))
	defer srv.Close()

	applied := make(chan QuotaSnapshot, 1)
	poller := &QuotaPoller{
		Client:   &GLMQuotaClient{BaseURL: srv.URL},
		Interval: time.Hour, // one immediate read, then idle
		Apply: func(s QuotaSnapshot) error {
			applied <- s
			return nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- poller.Run(ctx) }()
	select {
	case s := <-applied:
		if s.UsedPct != 10 || s.Source != "api" {
			t.Fatalf("snapshot: %+v", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no snapshot applied")
	}
	cancel()
	<-done
}

// TestQuotaPollerNoClientOnlyFallback: nil client means file-only mode.
func TestQuotaPollerNoClientOnlyFallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "glm-quota.json")
	if err := os.WriteFile(path, []byte(
		`{"limits":[{"type":"TOKENS_LIMIT","unit":6,"usagePct":12}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	applied := make(chan QuotaSnapshot, 1)
	poller := &QuotaPoller{Fallback: path, Interval: time.Hour, Apply: func(s QuotaSnapshot) error {
		applied <- s
		return nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	go poller.Run(ctx)
	select {
	case s := <-applied:
		if s.UsedPct != 12 {
			t.Fatalf("snapshot: %+v", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("file-only poller never applied")
	}
	cancel()
}

// TestQuotaBand walks the soft-band boundaries: <70 healthy, 70–95
// elevated, >95 critical, unknown (-1) empty.
func TestQuotaBand(t *testing.T) {
	cases := []struct {
		pct  float64
		want string
	}{
		{-1, ""}, {0, BandHealthy}, {69.9, BandHealthy},
		{70, BandElevated}, {95, BandElevated}, {95.1, BandCritical}, {100, BandCritical},
	}
	for _, tc := range cases {
		if got := QuotaBand(tc.pct); got != tc.want {
			t.Errorf("QuotaBand(%v) = %q, want %q", tc.pct, got, tc.want)
		}
	}
}

// TestAnnotateQuota: the band only annotates the reason — targets are
// untouched in M6-A, and unknown usage leaves the decision as-is.
func TestAnnotateQuota(t *testing.T) {
	d := Decide(EntrypointAuto, Request{}, nil, DefaultTargets())
	before := d.TargetModel
	d.AnnotateQuota(-1)
	if d.Reason != "classified by rule no-tools" {
		t.Fatalf("unknown usage must not annotate: %q", d.Reason)
	}
	d.AnnotateQuota(88)
	if !contains(d.Reason, BandElevated) || !contains(d.Reason, "88% used") {
		t.Fatalf("reason: %q", d.Reason)
	}
	if d.TargetModel != before {
		t.Fatalf("annotation must not change the target: %s -> %s", before, d.TargetModel)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

// TestLongContextTargets pins the glm-5.4[1m] branch: deep and routine
// oversize sessions move to the 1M variant, background stays on flash.
func TestLongContextTargets(t *testing.T) {
	targets := DefaultTargets()
	oversize := Request{ContextChars: LongContextTokens * 4 * 2} // ~2x threshold in tokens
	normal := Request{}

	for _, ep := range []string{EntrypointAuto, EntrypointDeep} {
		d := Decide(ep, Request{Thinking: true, ContextChars: oversize.ContextChars}, nil, targets)
		if d.TargetModel != LongContextTarget {
			t.Fatalf("%s deep oversize -> %s, want %s", ep, d.TargetModel, LongContextTarget)
		}
	}
	d := Decide(EntrypointAuto, oversize, nil, targets)
	if d.TargetModel != LongContextTarget {
		t.Fatalf("routine oversize -> %s, want %s", d.TargetModel, LongContextTarget)
	}
	if d := Decide(EntrypointBG, oversize, nil, targets); d.TargetModel != targets.Background {
		t.Fatalf("background must never move to %s: got %s", LongContextTarget, d.TargetModel)
	}
	if d := Decide(EntrypointAuto, normal, nil, targets); d.TargetModel != targets.Routine {
		t.Fatalf("normal routine: %+v", d)
	}
}

// TestLongContextThresholdConfigurable: the threshold lives on Targets so
// it can be retuned without code changes.
func TestLongContextThresholdConfigurable(t *testing.T) {
	targets := DefaultTargets()
	targets.LongContextTokens = 100
	d := Decide(EntrypointAuto, Request{ContextChars: 400}, nil, targets) // 100 tokens
	if d.TargetModel != LongContextTarget {
		t.Fatalf("custom threshold not honored: %+v", d)
	}
	targets.LongContextTokens = 0 // disabled
	if d := Decide(EntrypointAuto, Request{ContextChars: 1 << 20}, nil, targets); d.TargetModel != targets.Routine {
		t.Fatalf("threshold 0 must disable the override: %+v", d)
	}
}

// TestDeepSeekClientIsAvailable covers the balance probe against an
// httptest fake (Bearer auth, is_available flag, error status).
func TestDeepSeekClientIsAvailable(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.Header.Get("X-Down") == "1" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Write([]byte(`{"is_available":true}`))
	}))
	defer srv.Close()

	c := &DeepSeekClient{BaseURL: srv.URL, Key: func() (string, error) { return "ds-test", nil }}
	st, err := c.IsAvailable(context.Background())
	if err != nil || !st.Available {
		t.Fatalf("status: %+v err %v", st, err)
	}
	if gotAuth != "Bearer ds-test" {
		t.Fatalf("auth header: %q", gotAuth)
	}
	if st.CheckedAt.IsZero() {
		t.Fatal("CheckedAt must be stamped")
	}

	c = &DeepSeekClient{BaseURL: srv.URL, Key: func() (string, error) { return "k", nil },
		HTTP: &http.Client{Transport: headerTransport{base: http.DefaultTransport, name: "X-Down", value: "1"}}}
	if _, err := c.IsAvailable(context.Background()); err == nil {
		t.Fatal("non-200 must error")
	}
}
