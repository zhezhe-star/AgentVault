package auto

// Owner decision-channel event file (design §7, e247): the proxy appends
// one JSON line per pending-decision event to a fixed, configurable path
// so Orca/automation can watch for "something needs a decision". Lines
// carry metadata only — no keys, no request bodies (structural no-secret
// logging, same discipline as the request logger).

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Event types written to the event file.
const (
	EventPendingCreated = "pending_decision_created"
	EventPendingDecided = "pending_decision_decided"
)

// Event is one line of the event file. Metadata only by construction.
type Event struct {
	Time      string  `json:"ts"`
	Type      string  `json:"type"`
	ID        int64   `json:"id,omitempty"` // pending_decision row id
	Tier      string  `json:"task_tier,omitempty"`
	Provider  string  `json:"provider,omitempty"`
	Model     string  `json:"model,omitempty"`
	Decision  string  `json:"decision,omitempty"` // approved | denied
	EstTokens int64   `json:"est_tokens,omitempty"`
	EstCost   float64 `json:"est_cost,omitempty"`
	Reason    string  `json:"reason,omitempty"`
}

// DefaultEventPath is the standard event-file location: inside the data
// directory (design §7: ~/.config/agentvault/auto-events.jsonl).
func DefaultEventPath(dataDir string) string {
	return filepath.Join(dataDir, "auto-events.jsonl")
}

// EventLog appends events as JSON lines. A nil *EventLog or empty path
// disables writing (tests / notification face off).
type EventLog struct {
	mu   sync.Mutex
	path string
}

// NewEventLog builds an event log at path ("" disables it).
func NewEventLog(path string) *EventLog { return &EventLog{path: path} }

// Append writes one event line. Errors are returned but callers in the
// hot path are expected to degrade silently — a notification file must
// never take a request down.
func (l *EventLog) Append(e Event) error {
	if l == nil || l.path == "" {
		return nil
	}
	if e.Time == "" {
		e.Time = time.Now().UTC().Format(time.RFC3339Nano)
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}
