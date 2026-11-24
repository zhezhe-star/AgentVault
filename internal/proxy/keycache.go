package proxy

import (
	"errors"
	"sync"
	"time"

	"github.com/zhezhe-star/AgentVault/internal/secret"
)

// keyCacheTTL bounds how long a decrypted key stays in process memory.
// After expiry the next request re-reads it from the secret store, so a
// key rotated in the vault takes effect without a proxy restart.
const keyCacheTTL = 5 * time.Minute

// keyCache caches decrypted secrets keyed by provider/key-id with a TTL.
type keyCache struct {
	mu      sync.Mutex
	store   secret.Store
	service string
	entries map[string]cacheEntry
}

type cacheEntry struct {
	value     string
	expiresAt time.Time
}

func newKeyCache(store secret.Store, service string) *keyCache {
	return &keyCache{store: store, service: service, entries: map[string]cacheEntry{}}
}

// get returns the cached key or re-reads it from the secret store.
func (c *keyCache) get(providerID, keyID string) (string, error) {
	k := providerID + "/" + keyID
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[k]; ok && time.Now().Before(e.expiresAt) {
		return e.value, nil
	}
	v, err := c.store.Get(c.service, k)
	if err != nil {
		return "", err
	}
	c.entries[k] = cacheEntry{value: v, expiresAt: time.Now().Add(keyCacheTTL)}
	return v, nil
}

// invalidate drops a cached key (e.g. after a failure so rotation takes
// effect immediately).
func (c *keyCache) invalidate(providerID, keyID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, providerID+"/"+keyID)
}

// isNotFound reports whether err is the store's miss.
func isNotFound(err error) bool { return errors.Is(err, secret.ErrNotFound) }
