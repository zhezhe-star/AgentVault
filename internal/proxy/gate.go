package proxy

import (
	"context"
	"sync"
	"time"
)

// gate is the per-provider concurrency semaphore behind the routing
// decision. Slots are account-level: every key of one provider shares a
// single ceiling (two keys of one account do not double the upstream
// capacity), so admission is counted per provider, not per key.
//
// When every candidate provider is at capacity, acquire blocks FIFO: each
// provider keeps an ordered waiter list and release hands the freed slot
// to the head of the line, so no waiter can starve a later arrival.
type gate struct {
	mu       sync.Mutex
	inFlight map[string]int
	waiters  map[string][]chan struct{}
}

// gateStat is the observation snapshot for one provider.
type gateStat struct {
	InFlight int
	Queued   int
}

func newGate() *gate {
	return &gate{
		inFlight: map[string]int{},
		waiters:  map[string][]chan struct{}{},
	}
}

// tryAcquire takes a slot immediately, or reports the provider as full.
// limit <= 0 means unlimited (slots are still counted for observation).
func (g *gate) tryAcquire(providerID string, limit int) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if limit > 0 && g.inFlight[providerID] >= limit {
		return false
	}
	g.inFlight[providerID]++
	return true
}

// acquire takes a slot or joins the provider's FIFO queue until a slot is
// handed over, the deadline passes, or ctx is cancelled. A cancelled or
// timed-out waiter leaves the queue without holding a slot.
func (g *gate) acquire(ctx context.Context, providerID string, limit int, timeout time.Duration) bool {
	if g.tryAcquire(providerID, limit) {
		return true
	}
	ch := make(chan struct{}, 1)
	g.mu.Lock()
	// Re-check under the lock: a slot may have been released between the
	// failed tryAcquire and joining the queue.
	if limit > 0 && g.inFlight[providerID] < limit {
		g.inFlight[providerID]++
		g.mu.Unlock()
		return true
	}
	g.waiters[providerID] = append(g.waiters[providerID], ch)
	g.mu.Unlock()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ch:
		// Slot ownership was handed over by release (inFlight unchanged).
		return true
	case <-ctx.Done():
		if g.dequeue(providerID, ch) {
			return false
		}
		// Not in the queue anymore: the slot was already handed to us.
		g.release(providerID)
		return false
	case <-timer.C:
		if g.dequeue(providerID, ch) {
			return false
		}
		g.release(providerID)
		return false
	}
}

// dequeue removes a waiter from the queue; false means it was already
// handed a slot by release.
func (g *gate) dequeue(providerID string, ch chan struct{}) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	q := g.waiters[providerID]
	for i, c := range q {
		if c == ch {
			q = append(q[:i], q[i+1:]...)
			if len(q) == 0 {
				delete(g.waiters, providerID)
			} else {
				g.waiters[providerID] = q
			}
			return true
		}
	}
	return false
}

// release frees a slot. If anyone is queued, the slot goes straight to the
// head of the FIFO line (inFlight stays constant); otherwise it is
// returned to the pool.
func (g *gate) release(providerID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if q := g.waiters[providerID]; len(q) > 0 {
		head := q[0]
		q = q[1:]
		if len(q) == 0 {
			delete(g.waiters, providerID)
		} else {
			g.waiters[providerID] = q
		}
		head <- struct{}{} // buffered: never blocks
		return
	}
	if g.inFlight[providerID] > 0 {
		g.inFlight[providerID]--
	}
	if g.inFlight[providerID] == 0 {
		delete(g.inFlight, providerID)
	}
}

// stats snapshots in-flight and queued counts per provider (for
// /admin/routing and the status CLI). Empty map entries are omitted.
func (g *gate) stats() map[string]gateStat {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make(map[string]gateStat, len(g.inFlight)+len(g.waiters))
	for id, n := range g.inFlight {
		out[id] = gateStat{InFlight: n}
	}
	for id, q := range g.waiters {
		s := out[id]
		s.Queued = len(q)
		out[id] = s
	}
	return out
}
