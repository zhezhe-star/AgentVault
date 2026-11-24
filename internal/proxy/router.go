package proxy

import (
	"math"
	"sort"
	"sync"
	"time"

	"github.com/zhezhe-star/AgentVault/internal/provider"
)

// DefaultCooldown is how long a rate-limited key sits out before rejoining
// the rotation.
const DefaultCooldown = 60 * time.Second

// DefaultShortCooldown is the brief sit-out used when an upstream 429 looks
// like concurrency throttling (no Retry-After, no quota wording): the key
// typically frees up within seconds, so a full DefaultCooldown would
// needlessly starve single-key setups.
const DefaultShortCooldown = 2 * time.Second

// BreakerThreshold is the number of consecutive failures that trips the
// circuit breaker for a key.
const BreakerThreshold = 3

// BreakerOpenDuration is how long an open breaker keeps the key out. The
// first pick after expiry acts as the half-open probe: success closes the
// breaker, failure re-opens it.
const BreakerOpenDuration = 5 * time.Minute

// candidate is one routable upstream choice.
type candidate struct {
	providerID       string
	keyID            string
	baseURL          string
	anthropicBaseURL string
	weight           int
}

// router picks upstreams among the active keys serving a model, rotating
// round-robin and cooling down keys that fail.
type router struct {
	mu          sync.Mutex
	store       *provider.Store
	cooldown    time.Duration
	breakerOpen time.Duration // test hook; defaults to BreakerOpenDuration
	// cooldowns tracks keys currently sitting out, keyed by provider/key.
	cooldowns map[string]time.Time
	// fails counts consecutive failures per key; reaching
	// BreakerThreshold opens the breaker (a longer cooldown).
	fails map[string]int
	// breaker marks keys whose current cooldown is breaker-open rather
	// than a plain cooldown (for status observation).
	breaker map[string]bool
	// rr remembers the last-used index per model for round-robin.
	rr map[string]int
}

func newRouter(store *provider.Store, cooldown time.Duration) *router {
	return &router{
		store:       store,
		cooldown:    cooldown,
		breakerOpen: BreakerOpenDuration,
		cooldowns:   map[string]time.Time{},
		fails:       map[string]int{},
		breaker:     map[string]bool{},
		rr:          map[string]int{},
	}
}

// candidates returns every active key that serves the given model.
func (r *router) candidates(model string) ([]candidate, error) {
	models, err := r.store.ListAllModels()
	if err != nil {
		return nil, err
	}
	var out []candidate
	for _, m := range models {
		if m.Name != model {
			continue
		}
		keys, err := r.store.ListKeys(m.ProviderID)
		if err != nil {
			return nil, err
		}
		for _, k := range keys {
			if k.Status != provider.KeyActive {
				continue
			}
			p, err := r.store.GetProvider(m.ProviderID)
			if err != nil {
				continue
			}
			out = append(out, candidate{providerID: m.ProviderID, keyID: k.ID, baseURL: p.BaseURL, anthropicBaseURL: p.AnthropicBaseURL, weight: k.Weight})
		}
	}
	// Deterministic order: higher weight first, then key id as a stable tiebreak.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].weight != out[j].weight {
			return out[i].weight > out[j].weight
		}
		return out[i].keyID < out[j].keyID
	})
	return out, nil
}

// known reports whether any active key serves the model at all. pick
// returns false both for an unknown model and for an all-cooling model;
// callers use known to tell "no such model" (404) apart from "every key
// is cooling down" (429).
func (r *router) known(model string) bool {
	cands, err := r.candidates(model)
	return err == nil && len(cands) > 0
}

// upstreamName resolves the provider-side model id registered for a
// requested alias ("claude-opus-4-5" -> "glm-5.4"). It returns "" when the
// name is unknown or has no upstream mapping, so callers treat a non-empty,
// differing result as "rewrite the request body".
func (r *router) upstreamName(model string) string {
	models, err := r.store.ListAllModels()
	if err != nil {
		return ""
	}
	for _, m := range models {
		if m.Name == model && m.Upstream != "" {
			return m.Upstream
		}
	}
	return ""
}

// pick returns the next healthy candidate for the model, or false when all
// are cooling down (or none exist).
func (r *router) pick(model string) (candidate, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	cands, err := r.candidates(model)
	if err != nil || len(cands) == 0 {
		return candidate{}, false
	}
	now := time.Now()
	var healthy []candidate
	for _, c := range cands {
		id := c.providerID + "/" + c.keyID
		if until, hot := r.cooldowns[id]; hot {
			if now.Before(until) {
				continue // still cooling down
			}
			delete(r.cooldowns, id) // cooldown expired
		}
		healthy = append(healthy, c)
	}
	if len(healthy) == 0 {
		return candidate{}, false
	}
	// Round-robin among healthy candidates. rr maps model -> last used
	// index; the map's zero value 0 would skip the first candidate, so
	// treat an absent entry as "before the first".
	last, seen := r.rr[model]
	if !seen {
		last = -1
	}
	idx := (last + 1) % len(healthy)
	r.rr[model] = idx
	return healthy[idx], true
}

// failOption customizes a fail() cooldown without changing failure
// accounting.
type failOption func(*failOpts)

type failOpts struct {
	cooldown time.Duration
}

// withCooldown overrides the sit-out duration (e.g. a short cooldown for
// concurrency-throttle 429s, or the upstream's own Retry-After value).
func withCooldown(d time.Duration) failOption {
	return func(o *failOpts) { o.cooldown = d }
}

// fail puts a key into cooldown (429 or upstream 5xx). Consecutive
// failures at BreakerThreshold trip the breaker: the key sits out for
// BreakerOpenDuration instead, and its next pick is the half-open probe.
// Short cooldowns still count toward the breaker, so sustained rate
// limiting escalates to the long cooldown/open breaker as before.
func (r *router) fail(c candidate, opts ...failOption) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var o failOpts
	for _, f := range opts {
		f(&o)
	}
	id := c.providerID + "/" + c.keyID
	r.fails[id]++
	if r.fails[id] >= BreakerThreshold {
		r.cooldowns[id] = time.Now().Add(r.breakerOpen)
		r.breaker[id] = true
		return
	}
	d := r.cooldown
	if o.cooldown > 0 {
		d = o.cooldown
	}
	r.cooldowns[id] = time.Now().Add(d)
}

// success clears any failure state for the key (a successful half-open
// probe closes the breaker).
func (r *router) success(c candidate) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := c.providerID + "/" + c.keyID
	delete(r.cooldowns, id)
	delete(r.fails, id)
	delete(r.breaker, id)
}

// breakerCount reports how many keys currently have an open breaker.
func (r *router) breakerCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.breaker)
}

// retryAfter returns how long until the soonest cooling key rejoins the
// rotation, rounded up to whole seconds (minimum 1). It returns 0 when
// nothing is cooling, so callers only set Retry-After on a genuine
// all-keys-cooling 429.
func (r *router) retryAfter() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	var min time.Duration
	for _, until := range r.cooldowns {
		d := until.Sub(now)
		if d <= 0 {
			continue
		}
		if min == 0 || d < min {
			min = d
		}
	}
	if min == 0 {
		return 0
	}
	secs := int64(math.Ceil(min.Seconds()))
	if secs < 1 {
		secs = 1
	}
	return time.Duration(secs) * time.Second
}

// coolingCount reports how many keys are currently cooling down (for
// status/console views).
func (r *router) coolingCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	now := time.Now()
	for _, until := range r.cooldowns {
		if now.Before(until) {
			n++
		}
	}
	return n
}
