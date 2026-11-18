package proxy

import (
	"sort"
	"sync"
	"time"

	"github.com/zhezhe-star/AgentVault/internal/provider"
)

// DefaultCooldown is how long a rate-limited key sits out before rejoining
// the rotation.
const DefaultCooldown = 60 * time.Second

// candidate is one routable upstream choice.
type candidate struct {
	providerID string
	keyID      string
	baseURL    string
	weight     int
}

// router picks upstreams among the active keys serving a model, rotating
// round-robin and cooling down keys that fail.
type router struct {
	mu       sync.Mutex
	store    *provider.Store
	cooldown time.Duration
	// cooldowns tracks keys currently sitting out, keyed by provider/key.
	cooldowns map[string]time.Time
	// rr remembers the last-used index per model for round-robin.
	rr map[string]int
}

func newRouter(store *provider.Store, cooldown time.Duration) *router {
	return &router{
		store:     store,
		cooldown:  cooldown,
		cooldowns: map[string]time.Time{},
		rr:        map[string]int{},
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
			out = append(out, candidate{providerID: m.ProviderID, keyID: k.ID, baseURL: p.BaseURL, weight: k.Weight})
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

// fail puts a key into cooldown (429 or upstream 5xx).
func (r *router) fail(c candidate) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cooldowns[c.providerID+"/"+c.keyID] = time.Now().Add(r.cooldown)
}

// success clears any failure state for the key.
func (r *router) success(c candidate) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.cooldowns, c.providerID+"/"+c.keyID)
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
