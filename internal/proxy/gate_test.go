package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zhezhe-star/AgentVault/internal/provider"
)

// The gate is exercised through the router, the same way the proxy uses it.

// At-limit providers are skipped in favor of a healthy sibling provider
// serving the same model.
func TestGateOverLimitProviderSkipped(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"id":"1","object":"chat.completion","choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer upstream.Close()
	store, sec, _ := fixture(t, upstream.URL)
	// Second provider serving the same model name.
	if err := store.CreateProvider(provider.Provider{ID: "prov-b", Name: "B", BaseURL: upstream.URL}); err != nil {
		t.Fatalf("CreateProvider b: %v", err)
	}
	if err := store.CreateKey(provider.Key{ID: "b1", ProviderID: "prov-b", SecretID: "ref-b", Status: provider.KeyActive}); err != nil {
		t.Fatalf("CreateKey b: %v", err)
	}
	if err := sec.Set("agentvault", "prov-b/b1", testKey); err != nil {
		t.Fatalf("secret set b: %v", err)
	}
	if err := store.CreateModel(provider.Model{ID: "mb1", ProviderID: "prov-b", KeyID: "b1", Name: "test-model", Upstream: "test-model"}); err != nil {
		t.Fatalf("CreateModel b: %v", err)
	}

	r := newRouter(store, DefaultCooldown)
	// Fill the first provider's single slot.
	if err := store.SetConcurrency("mockprov", 1); err != nil {
		t.Fatalf("SetConcurrency: %v", err)
	}
	c, release, st := r.acquire(context.Background(), "test-model", time.Second)
	if st != acquireOK {
		t.Fatalf("first acquire status = %v, want OK", st)
	}
	if c.providerID != "mockprov" && c.providerID != "prov-b" {
		t.Fatalf("unexpected provider %q", c.providerID)
	}
	first := c.providerID
	release()

	// Hold the first provider's slot again (deterministic: acquire until we
	// hold a mockprov slot), then the next acquire must land on prov-b.
	for i := 0; i < 4; i++ {
		c, rel, st := r.acquire(context.Background(), "test-model", time.Second)
		if st != acquireOK {
			t.Fatalf("acquire %d status = %v", i, st)
		}
		if c.providerID == first {
			// Keep this one held; the sibling provider is the free option.
			defer rel()
			c2, rel2, st2 := r.acquire(context.Background(), "test-model", time.Second)
			if st2 != acquireOK {
				t.Fatalf("second acquire status = %v, want sibling provider", st2)
			}
			if c2.providerID == first {
				t.Fatalf("acquire landed on full provider %q; limit not enforced", first)
			}
			rel2()
			return
		}
		rel()
	}
	t.Fatal("never acquired a slot on the limited provider")
}

// FIFO: queued waiters are served strictly in arrival order — each release
// hands the slot to the head of the queue.
func TestGateFIFOOrder(t *testing.T) {
	store, _, _ := fixture(t, "http://127.0.1:1")
	if err := store.SetConcurrency("mockprov", 1); err != nil {
		t.Fatalf("SetConcurrency: %v", err)
	}
	r := newRouter(store, DefaultCooldown)

	_, release, st := r.acquire(context.Background(), "test-model", time.Second)
	if st != acquireOK {
		t.Fatalf("initial acquire status = %v", st)
	}

	// Enqueue three waiters through the router; each is confirmed queued
	// (gate stats) before the next starts, so arrival order is a fact.
	const waiters = 3
	granted := make(chan int, waiters)
	var wg sync.WaitGroup
	for i := 0; i < waiters; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, rel, st := r.acquire(context.Background(), "test-model", 5*time.Second)
			if st != acquireOK {
				t.Errorf("waiter %d status = %v", i, st)
				return
			}
			granted <- i
			rel()
		}(i)
		deadline := time.After(2 * time.Second)
		for {
			if r.gateStats()["mockprov"].Queued >= i+1 {
				break
			}
			select {
			case <-deadline:
				t.Fatalf("waiter %d never queued; stats %+v", i, r.gateStats()["mockprov"])
			case <-time.After(5 * time.Millisecond):
			}
		}
	}

	// Free the slot: the release hands it to waiter 0, whose rel() hands it
	// to waiter 1, and so on — grants must arrive in exactly [0 1 2] order.
	release()
	order := []int{<-granted, <-granted, <-granted}
	for i, got := range order {
		if got != i {
			t.Fatalf("FIFO violated: grant order %v, want [0 1 2]", order)
		}
	}
	wg.Wait()
	if g := r.gateStats()["mockprov"]; g.InFlight != 0 || g.Queued != 0 {
		t.Fatalf("gate not drained after releases: %+v", g)
	}
}

// Waiting past the queue budget answers with a queue-timeout status (the
// HTTP layer turns this into 429 + Retry-After).
func TestGateQueueTimeout(t *testing.T) {
	store, _, _ := fixture(t, "http://127.0.1:1")
	if err := store.SetConcurrency("mockprov", 1); err != nil {
		t.Fatalf("SetConcurrency: %v", err)
	}
	r := newRouter(store, DefaultCooldown)
	_, release, st := r.acquire(context.Background(), "test-model", time.Second)
	if st != acquireOK {
		t.Fatalf("initial acquire status = %v", st)
	}
	defer release()

	start := time.Now()
	_, _, st = r.acquire(context.Background(), "test-model", 100*time.Millisecond)
	if st != acquireQueuedTimeout {
		t.Fatalf("status = %v, want queuedTimeout", st)
	}
	if elapsed := time.Since(start); elapsed < 80*time.Millisecond {
		t.Fatalf("returned after %v; timeout not honored", elapsed)
	}
	// The timed-out waiter must not hold or corrupt the slot count.
	if g := r.gateStats()["mockprov"]; g.InFlight != 1 || g.Queued != 0 {
		t.Fatalf("after timeout: %+v, want inFlight=1 queued=0", g)
	}
}

// A cancelled client leaves the queue without taking a slot, and later
// acquires still work exactly as before.
func TestGateClientCancelDoesNotHoldSlot(t *testing.T) {
	store, _, _ := fixture(t, "http://127.0.1:1")
	if err := store.SetConcurrency("mockprov", 1); err != nil {
		t.Fatalf("SetConcurrency: %v", err)
	}
	r := newRouter(store, DefaultCooldown)
	_, release, st := r.acquire(context.Background(), "test-model", time.Second)
	if st != acquireOK {
		t.Fatalf("initial acquire status = %v", st)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan acquireStatus, 1)
	go func() {
		_, _, st := r.acquire(ctx, "test-model", 10*time.Second)
		done <- st
	}()
	time.Sleep(50 * time.Millisecond) // let the waiter enqueue
	cancel()
	if st := <-done; st != acquireCancelled {
		t.Fatalf("status = %v, want cancelled", st)
	}
	if g := r.gateStats()["mockprov"]; g.InFlight != 1 || g.Queued != 0 {
		t.Fatalf("after cancel: %+v, want inFlight=1 queued=0 (no slot taken)", g)
	}
	release()
	// Slot accounting intact: another acquire succeeds immediately.
	if _, rel, st := r.acquire(context.Background(), "test-model", time.Second); st != acquireOK {
		t.Fatalf("post-cancel acquire status = %v, want OK", st)
	} else {
		rel()
	}
}

// Integration: with the account ceiling at N, the mock upstream never sees
// more than N concurrent requests, and queued excess requests are answered
// successfully as slots free up.
func TestIntegrationUpstreamConcurrencyCapped(t *testing.T) {
	var cur, max int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&cur, 1)
		for {
			old := atomic.LoadInt32(&max)
			if n <= old || atomic.CompareAndSwapInt32(&max, old, n) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond) // widen the overlap window
		atomic.AddInt32(&cur, -1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"1","object":"chat.completion","choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer upstream.Close()

	store, sec, svc := fixture(t, upstream.URL)
	if err := store.SetConcurrency("mockprov", 2); err != nil {
		t.Fatalf("SetConcurrency: %v", err)
	}
	// A second key on the same provider: the ceiling is account-level, so
	// it must NOT raise the upstream concurrency to 4.
	if err := store.CreateKey(provider.Key{ID: "k2", ProviderID: "mockprov", SecretID: "ref2", Status: provider.KeyActive}); err != nil {
		t.Fatalf("CreateKey k2: %v", err)
	}
	if err := sec.Set("agentvault", "mockprov/k2", testKey); err != nil {
		t.Fatalf("secret set k2: %v", err)
	}
	if err := store.CreateModel(provider.Model{ID: "m2", ProviderID: "mockprov", KeyID: "k2", Name: "test-model", Upstream: "test-model"}); err != nil {
		t.Fatalf("CreateModel k2: %v", err)
	}

	srv := New(store, sec, svc)
	srv.SetQueueTimeout(10 * time.Second)
	base := startProxy(t, srv)

	const total = 8
	var wg sync.WaitGroup
	statuses := make([]int, total)
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := http.Post(base+"/v1/chat/completions", "application/json",
				strings.NewReader(`{"model":"test-model","messages":[]}`))
			if err != nil {
				t.Errorf("post %d: %v", i, err)
				return
			}
			statuses[i] = resp.StatusCode
			resp.Body.Close()
		}(i)
	}
	wg.Wait()

	if got := atomic.LoadInt32(&max); got > 2 {
		t.Fatalf("upstream saw %d concurrent requests, want <= 2 (account ceiling)", got)
	}
	for i, sc := range statuses {
		if sc != http.StatusOK {
			t.Fatalf("request %d status = %d, want 200 (it should queue, not fail)", i, sc)
		}
	}
	if g := srv.router.gateStats()["mockprov"]; g.InFlight != 0 || g.Queued != 0 {
		t.Fatalf("gate not drained after all responses: %+v", g)
	}
}

// The slot is held for the whole streaming response and freed only after
// the stream is fully written back — and the /admin/routing counters
// reflect in-flight vs queued.
func TestIntegrationSlotHeldUntilStreamEnds(t *testing.T) {
	streamDone := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		for i := 0; i < 3; i++ {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"c\"}}]}\n\n")
			f.Flush()
			time.Sleep(30 * time.Millisecond)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		close(streamDone)
	}))
	defer upstream.Close()

	store, sec, svc := fixture(t, upstream.URL)
	if err := store.SetConcurrency("mockprov", 1); err != nil {
		t.Fatalf("SetConcurrency: %v", err)
	}
	srv := New(store, sec, svc)
	srv.SetQueueTimeout(5 * time.Second)
	base := startProxy(t, srv)

	req, _ := http.NewRequest(http.MethodPost, base+"/v1/chat/completions",
		strings.NewReader(`{"model":"test-model","stream":true,"messages":[]}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	// While the stream is open the slot is held: a probe acquire must
	// report the provider as in-flight.
	deadline := time.After(2 * time.Second)
	for {
		g := srv.router.gateStats()["mockprov"]
		if g.InFlight == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("slot never observed in-flight; stats %+v", g)
		case <-time.After(10 * time.Millisecond):
		}
	}

	// Draining the streamed body must complete before the slot frees.
	buf := make([]byte, 4096)
	for {
		_, err := resp.Body.Read(buf)
		if err != nil {
			break
		}
	}
	select {
	case <-streamDone:
	default:
		t.Fatal("stream not finished after client drained")
	}
	// flushCopy returns after the upstream body is done; give the proxy a
	// beat to run the release, then the gate must be empty.
	deadline2 := time.After(2 * time.Second)
	for {
		g := srv.router.gateStats()["mockprov"]
		if g.InFlight == 0 && g.Queued == 0 {
			return // released only after the stream ended: pass
		}
		select {
		case <-deadline2:
			t.Fatalf("slot not released after stream end: %+v", g)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// End to end: a request that waits out the whole queue budget is answered
// with 429 + Retry-After and a "queued too long" message (distinguishable
// from the cooldown 429), while the first request still succeeds.
func TestIntegrationQueueTimeoutReturns429(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(400 * time.Millisecond)
		fmt.Fprint(w, `{"id":"1","object":"chat.completion","choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer upstream.Close()
	store, sec, svc := fixture(t, upstream.URL)
	if err := store.SetConcurrency("mockprov", 1); err != nil {
		t.Fatalf("SetConcurrency: %v", err)
	}
	srv := New(store, sec, svc)
	srv.SetQueueTimeout(100 * time.Millisecond)
	base := startProxy(t, srv)

	first := make(chan int, 1)
	go func() {
		resp, err := http.Post(base+"/v1/chat/completions", "application/json",
			strings.NewReader(`{"model":"test-model","messages":[]}`))
		if err != nil {
			t.Errorf("first post: %v", err)
			first <- 0
			return
		}
		first <- resp.StatusCode
		resp.Body.Close()
	}()
	time.Sleep(50 * time.Millisecond) // let the first request take the slot

	queued := postChat(t, base, "test-model")
	defer queued.Body.Close()
	if queued.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("queued-too-long status = %d, want 429", queued.StatusCode)
	}
	if ra := queued.Header.Get("Retry-After"); ra == "" {
		t.Fatal("queued-too-long 429 missing Retry-After")
	}
	body := map[string]any{}
	if err := json.NewDecoder(queued.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	errObj := body["error"].(map[string]any)
	if msg, _ := errObj["message"].(string); !strings.Contains(msg, "queued too long") {
		t.Fatalf("message = %q, want queue wording", msg)
	}
	if sc := <-first; sc != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", sc)
	}
}

// The admin endpoint surfaces per-provider in-flight and queued counts.
func TestAdminRoutingReportsGateCounts(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		fmt.Fprint(w, `{"id":"1","object":"chat.completion","choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer upstream.Close()
	store, sec, svc := fixture(t, upstream.URL)
	if err := store.SetConcurrency("mockprov", 1); err != nil {
		t.Fatalf("SetConcurrency: %v", err)
	}
	srv := New(store, sec, svc)
	srv.SetQueueTimeout(5 * time.Second)
	base := startProxy(t, srv)

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ { // 1 in flight, 2 queued
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Post(base+"/v1/chat/completions", "application/json",
				strings.NewReader(`{"model":"test-model","messages":[]}`))
			if err == nil {
				resp.Body.Close()
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)

	resp, err := http.Get(base + "/admin/routing")
	if err != nil {
		t.Fatalf("admin routing: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		Providers []struct {
			ID       string `json:"id"`
			InFlight int    `json:"in_flight"`
			Queued   int    `json:"queued"`
		} `json:"providers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	wg.Wait()
	var ps *struct {
		ID       string `json:"id"`
		InFlight int    `json:"in_flight"`
		Queued   int    `json:"queued"`
	}
	for i := range out.Providers {
		if out.Providers[i].ID == "mockprov" {
			ps = &out.Providers[i]
		}
	}
	if ps == nil {
		t.Fatalf("mockprov missing from admin output: %+v", out.Providers)
	}
	if ps.InFlight+ps.Queued != 3 {
		t.Fatalf("in_flight=%d queued=%d, want 3 total (1+2)", ps.InFlight, ps.Queued)
	}
}
