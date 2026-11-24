package proxy

import (
	"time"

	"github.com/zhezhe-star/AgentVault/internal/provider"
	"github.com/zhezhe-star/AgentVault/internal/usage"
)

// quotaChecker enforces rolling-window token budgets per provider and per
// key. It reads the live usage records, so limits apply across proxy
// restarts (the window is computed from real history).
type quotaChecker struct {
	providers *provider.Store
	usage     *usage.Store
}

// overLimit returns the first quota the request would exceed for this
// route, or nil.
func (q *quotaChecker) overLimit(c candidate) *provider.Quota {
	if q == nil {
		return nil
	}
	quotas, err := q.providers.ListQuotas()
	if err != nil || len(quotas) == 0 {
		return nil
	}
	now := time.Now()
	for i := range quotas {
		qt := &quotas[i]
		var since time.Time
		switch qt.Scope {
		case "provider":
			if qt.RefID != c.providerID {
				continue
			}
			since = now.Add(-time.Duration(qt.WindowSeconds) * time.Second)
			in, out, err := q.usage.SumTokensSince(c.providerID, "", since)
			if err != nil || in+out >= qt.MaxTokens {
				return qt
			}
		case "key":
			if qt.RefID != c.keyID {
				continue
			}
			since = now.Add(-time.Duration(qt.WindowSeconds) * time.Second)
			in, out, err := q.usage.SumTokensSince(c.providerID, c.keyID, since)
			if err != nil || in+out >= qt.MaxTokens {
				return qt
			}
		}
	}
	return nil
}
