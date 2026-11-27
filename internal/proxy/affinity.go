package proxy

// Session cache affinity (design §8, M6-4): a per-session binding of
// (provider, key, model) kept in process memory with a TTL — never
// persisted. A bound session whose target is still healthy stays on it
// (prefix-cache preservation); the binding dies on TTL expiry or when the
// target cools/breaks.

import (
	"net/http"
	"regexp"
	"sync"
	"time"
)

// DefaultAffinityTTL is how long a session binding survives without being
// seen again (design §8: 2h).
const DefaultAffinityTTL = 2 * time.Hour

// sessionHeaderPattern guards against junk values: at least 8 chars of
// alphanumerics, hyphens or underscores.
var sessionHeaderPattern = regexp.MustCompile(`^[\w-]{8,}$`)

// validSessionID reports whether a header value looks like a session id.
func validSessionID(v string) bool { return sessionHeaderPattern.MatchString(v) }

// SessionID extracts the client session id from request headers, by
// priority (design §8): Claude Code's x-claude-code-session-id first, then
// OpenCode (the parent session id wins over the child's own — child agents
// share the parent's cache), then the generic session-id. "" when no
// recognizable session.
func SessionID(h http.Header) string {
	for _, name := range []string{"X-Claude-Code-Session-Id", "X-Parent-Session-Id", "X-Session-Id", "Session-Id"} {
		if v := h.Get(name); v != "" && validSessionID(v) {
			return v
		}
	}
	return ""
}

// affinityBinding is one session's sticky upstream.
type affinityBinding struct {
	providerID string
	keyID      string
	model      string
	expiresAt  time.Time
}

// sessionAffinity is the in-memory session → (provider, key, model) table.
type sessionAffinity struct {
	mu     sync.Mutex
	ttl    time.Duration
	now    func() time.Time
	m      map[string]affinityBinding
	router *router // health checks (cooldown/breaker) for the bound target
}

func newSessionAffinity(rt *router, ttl time.Duration) *sessionAffinity {
	if ttl <= 0 {
		ttl = DefaultAffinityTTL
	}
	return &sessionAffinity{ttl: ttl, now: time.Now, m: map[string]affinityBinding{}, router: rt}
}

// lookup returns the session's binding if it exists, is within TTL, and
// its target is still healthy (not cooling / not breaker-open). Expired or
// unhealthy entries are dropped so the next request rebinds fresh.
func (a *sessionAffinity) lookup(session string) (affinityBinding, bool) {
	a.mu.Lock()
	b, ok := a.m[session]
	if ok && !a.now().Before(b.expiresAt) {
		delete(a.m, session)
		ok = false
	}
	a.mu.Unlock()
	if !ok {
		return affinityBinding{}, false
	}
	// Health check outside the affinity lock (router has its own lock).
	if !a.router.bindingHealthy(b.model, b.providerID, b.keyID) {
		a.mu.Lock()
		delete(a.m, session)
		a.mu.Unlock()
		return affinityBinding{}, false
	}
	return b, true
}

// bind records (or refreshes) the session's sticky upstream.
func (a *sessionAffinity) bind(session, providerID, keyID, model string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.m[session] = affinityBinding{
		providerID: providerID, keyID: keyID, model: model,
		expiresAt: a.now().Add(a.ttl),
	}
}

// size reports the live binding count (tests/observation).
func (a *sessionAffinity) size() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.m)
}
